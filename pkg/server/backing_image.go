package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/longhorn/sparse-tools/sparse"
	sparserest "github.com/longhorn/sparse-tools/sparse/rest"

	"github.com/longhorn/backing-image-manager/pkg/rpc"
	"github.com/longhorn/backing-image-manager/pkg/types"
	"github.com/longhorn/backing-image-manager/pkg/util"
)

type state string

const (
	StatePending     = state(types.DownloadStatePending)
	StateDownloading = state(types.DownloadStateDownloading)
	StateDownloaded  = state(types.DownloadStateDownloaded)
	StateFailed      = state(types.DownloadStateFailed)
)

type BackingImage struct {
	Name          string
	URL           string
	UUID          string
	HostDirectory string
	WorkDirectory string
	state         state
	errorMsg      string

	size              int64
	processedSize     int64
	progress          int
	downloadCanceller context.CancelFunc

	sendingReference     int
	senderManagerAddress string

	// Need to acquire lock when access to BackingImage fields as well as its meta file.
	lock *sync.RWMutex

	log      logrus.FieldLogger
	updateCh chan interface{}
}

func NewBackingImage(name, url, uuid, diskPathOnHost string) *BackingImage {
	hostDir := filepath.Join(diskPathOnHost, types.BackingImageDirectoryName, GetBackingImageDirectoryName(name, uuid))
	workDir := filepath.Join(types.WorkDirectory, GetBackingImageDirectoryName(name, uuid))
	return &BackingImage{
		Name:          name,
		UUID:          uuid,
		URL:           url,
		HostDirectory: hostDir,
		WorkDirectory: workDir,
		state:         StatePending,
		log: logrus.StandardLogger().WithFields(
			logrus.Fields{
				"component": "backing-image",
				"name":      name,
				"url":       url,
				"uuid":      uuid,
				"hostDir":   hostDir,
				"workDir":   workDir,
			},
		),
		lock: &sync.RWMutex{},
	}
}

func GetBackingImageDirectoryName(biName, biUUID string) string {
	return fmt.Sprintf("%s-%s", biName, biUUID)
}

func (bi *BackingImage) SetUpdateChannel(updateCh chan interface{}) {
	bi.updateCh = updateCh
}

func IntroduceDownloadedBackingImage(name, url, uuid, diskPathOnHost string, size int64) *BackingImage {
	bi := NewBackingImage(name, url, uuid, diskPathOnHost)
	bi.size = size
	if name == "" || uuid == "" || diskPathOnHost == "" || size <= 0 {
		bi.state = types.DownloadStateFailed
	} else {
		bi.state = types.DownloadStateDownloaded
	}
	return bi
}

func (bi *BackingImage) Pull() (resp *rpc.BackingImageResponse, err error) {
	bi.lock.Lock()
	defer func() {
		if err != nil {
			bi.state = StateFailed
			bi.errorMsg = err.Error()
			bi.log.WithError(err).Error("Backing Image: failed to pull backing image")
		}
		bi.lock.Unlock()
		bi.updateCh <- nil
	}()
	bi.log.Info("Backing Image: start to pull backing image")

	if err = bi.checkAndReuseBackingImageFileWithoutLock(); err == nil {
		bi.log.Infof("Backing Image: succeeded to reuse the existing backing image file, will skip pulling")
		return bi.rpcResponse(), nil
	}
	bi.log.Infof("Backing Image: failed to try to check or reuse the possible existing backing image file, will start pulling then: %v", err)

	if err := bi.prepareForDownload(); err != nil {
		return nil, errors.Wrapf(err, "failed to prepare for pulling")
	}

	size, err := util.GetDownloadSize(bi.URL)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get file size before pulling")
	}
	if size <= 0 {
		bi.log.Warnf("Backing Image: cannot get size from URL, will set size after pulling")
	}
	ctx, cancel := context.WithCancel(context.Background())
	bi.size = size
	bi.downloadCanceller = cancel

	go func() {
		defer func() {
			bi.updateCh <- nil
		}()

		written, err := util.DownloadFile(ctx, cancel, bi.URL, filepath.Join(bi.WorkDirectory, types.BackingImageTmpFileName), bi)
		if err != nil {
			bi.lock.Lock()
			bi.downloadCanceller = nil
			bi.state = StateFailed
			bi.errorMsg = err.Error()
			bi.log.WithError(err).Error("Backing Image: failed to pull from remote")
			bi.lock.Unlock()
			return
		}
		bi.completeDownloadWithLock(written)
		return
	}()

	bi.log.Info("Backing Image: pulling backing image")

	return bi.rpcResponse(), nil
}

func (bi *BackingImage) Delete() (err error) {
	bi.lock.Lock()
	oldState := bi.state
	defer func() {
		currentState := bi.state
		if bi.downloadCanceller != nil {
			bi.downloadCanceller()
			bi.downloadCanceller = nil
		}
		bi.lock.Unlock()
		if oldState != currentState {
			bi.updateCh <- nil
		}
	}()

	bi.log.Info("Backing Image: start to clean up backing image")

	if err := os.RemoveAll(bi.WorkDirectory); err != nil {
		err = errors.Wrapf(err, "failed to clean up work directory %v when deleting the backing image", bi.WorkDirectory)
		bi.state = StateFailed
		bi.errorMsg = err.Error()
		bi.log.WithError(err).Error("Backing Image: failed to do cleanup")
		return err
	}

	bi.log.Info("Backing Image: cleanup succeeded")

	return nil
}

func (bi *BackingImage) Get() (resp *rpc.BackingImageResponse, err error) {
	bi.lock.Lock()
	oldState := bi.state
	defer func() {
		if err != nil {
			bi.state = StateFailed
			bi.errorMsg = err.Error()
			if bi.downloadCanceller != nil {
				bi.downloadCanceller()
				bi.downloadCanceller = nil
			}
			bi.log.WithError(err).Error("Backing Image: failed to get backing image")
		}

		currentState := bi.state
		bi.lock.Unlock()
		if oldState != currentState {
			bi.updateCh <- nil
		}
	}()

	if err := bi.validateFiles(); err != nil {
		return nil, err
	}

	if bi.state == types.DownloadStateDownloaded && bi.size <= 0 {
		return nil, fmt.Errorf("invalid size %v for downloaded file", bi.size)
	}

	return bi.rpcResponse(), nil
}

func (bi *BackingImage) Receive(size int64, senderManagerAddress string, portAllocateFunc func(portCount int32) (int32, int32, error), portReleaseFunc func(start, end int32) error) (port int32, err error) {
	bi.lock.Lock()
	defer func() {
		if err != nil {
			bi.state = StateFailed
			bi.errorMsg = err.Error()
			bi.log.WithError(err).Error("Backing Image: failed to receive backing image")
		}
		bi.lock.Unlock()
		bi.updateCh <- nil
	}()

	bi.senderManagerAddress = senderManagerAddress
	bi.log = bi.log.WithField("senderManagerAddress", senderManagerAddress)

	if err = bi.checkAndReuseBackingImageFileWithoutLock(); err == nil {
		bi.log.Infof("Backing Image: succeeded to reuse the existing backing image file, will skip syncing")
		return 0, nil
	}
	bi.log.Infof("Backing Image: failed to try to check or reuse the possible existing backing image file, will start syncing then: %v", err)

	if err := bi.prepareForDownload(); err != nil {
		return 0, errors.Wrapf(err, "failed to prepare for backing image receiving")
	}

	if port, _, err = portAllocateFunc(1); err != nil {
		return 0, errors.Wrapf(err, "failed to request a port for backing image receiving")
	}

	bi.size = size

	go func() {
		defer func() {
			bi.updateCh <- nil
			if err := portReleaseFunc(port, port+1); err != nil {
				bi.log.WithError(err).Errorf("Failed to release port %v after receiving backing image", port)
			}
		}()

		bi.log.Infof("Backing Image: prepare to receive backing image at port %v", port)

		if err := sparserest.Server(strconv.Itoa(int(port)), filepath.Join(bi.WorkDirectory, types.BackingImageTmpFileName), bi); err != nil && err != http.ErrServerClosed {
			bi.lock.Lock()
			bi.state = StateFailed
			bi.errorMsg = err.Error()
			bi.log.WithError(err).Errorf("Backing Image: failed to receive backing image from %v", senderManagerAddress)
			bi.lock.Unlock()
			return
		}
		bi.completeDownloadWithLock(size)
		return
	}()

	return port, nil
}

func (bi *BackingImage) Send(address string, portAllocateFunc func(portCount int32) (int32, int32, error), portReleaseFunc func(start, end int32) error) (err error) {
	bi.lock.Lock()
	oldState := bi.state
	defer func() {
		currentState := bi.state
		bi.lock.Unlock()
		if oldState != currentState {
			bi.updateCh <- nil
		}
	}()

	if bi.state != types.DownloadStateDownloaded {
		return fmt.Errorf("backing image %v with state %v is invalid for file sending", bi.Name, bi.state)
	}
	if err := bi.validateFiles(); err != nil {
		bi.state = StateFailed
		bi.errorMsg = err.Error()
		bi.log.WithError(err).Error("Backing Image: failed to validate files before sending")
		return errors.Wrapf(err, "cannot send backing image %v to others since the files are invalid", bi.Name)
	}
	if bi.sendingReference >= types.SendingLimit {
		return fmt.Errorf("backing image %v is already sending data to %v backing images", bi.Name, types.SendingLimit)
	}

	port, _, err := portAllocateFunc(1)
	if err != nil {
		return errors.Wrapf(err, "failed to request a port for backing image sending")
	}

	bi.sendingReference++

	go func() {
		bi.log.Infof("Backing Image: start to send backing image to address %v", address)
		defer func() {
			bi.lock.Lock()
			bi.sendingReference--
			bi.lock.Unlock()
			bi.updateCh <- nil
			if err := portReleaseFunc(port, port+1); err != nil {
				bi.log.WithError(err).Errorf("Failed to release port %v after sending backing image", port)
			}
		}()

		if err := sparse.SyncFile(filepath.Join(bi.WorkDirectory, types.BackingImageFileName), address, types.FileSyncTimeout, false); err != nil {
			bi.log.WithError(err).Errorf("Backing Image: failed to send backing image to address %v", address)
			return
		}
		bi.log.Infof("Backing Image: done sending backing image to address %v", address)
	}()

	return nil
}

func (bi *BackingImage) rpcResponse() *rpc.BackingImageResponse {
	resp := &rpc.BackingImageResponse{
		Spec: &rpc.BackingImageSpec{
			Name:      bi.Name,
			Url:       bi.URL,
			Uuid:      bi.UUID,
			Size:      bi.size,
			Directory: bi.HostDirectory,
		},

		Status: &rpc.BackingImageStatus{
			State:                string(bi.state),
			SendingReference:     int32(bi.sendingReference),
			ErrorMsg:             bi.errorMsg,
			SenderManagerAddress: bi.senderManagerAddress,
			DownloadProgress:     int32(bi.progress),
		},
	}
	return resp
}

func (bi *BackingImage) checkAndReuseBackingImageFileWithoutLock() error {
	backingImagePath := filepath.Join(bi.WorkDirectory, types.BackingImageFileName)
	info, err := os.Stat(backingImagePath)
	if err != nil {
		return err
	}
	cfg, err := util.ReadBackingImageConfigFile(bi.WorkDirectory)
	if err != nil {
		return err
	}
	if info.Size() != cfg.Size || bi.Name != cfg.Name || bi.UUID != bi.UUID || bi.URL != bi.URL {
		return fmt.Errorf("backing image config %+v doesn't match the backing image current status or actual file size %v", cfg, info.Size())
	}

	bi.size = cfg.Size
	bi.processedSize = cfg.Size
	bi.progress = 100
	bi.state = types.DownloadStateDownloaded
	bi.log.Infof("Backing Image: Directly reuse the existing file in path %v", backingImagePath)

	return nil
}

func (bi *BackingImage) prepareForDownload() error {
	if _, err := os.Stat(bi.WorkDirectory); os.IsNotExist(err) {
		if err := os.Mkdir(bi.WorkDirectory, 666); err != nil {
			return errors.Wrapf(err, "failed to create work directory %v before downloading", bi.WorkDirectory)
		}
		return nil
	}

	configFilePath := filepath.Join(bi.WorkDirectory, util.BackingImageConfigFile)
	if err := os.Remove(configFilePath); err != nil && !os.IsNotExist(err) {
		return err
	}

	// By renaming the existing backing image file to the tmp file, the sync function can reuse part of the data
	backingImageTmpPath := filepath.Join(bi.WorkDirectory, types.BackingImageTmpFileName)
	backingImagePath := filepath.Join(bi.WorkDirectory, types.BackingImageFileName)
	if _, err := os.Stat(backingImagePath); os.IsExist(err) {
		if _, err := os.Stat(backingImageTmpPath); os.IsExist(err) {
			if err := os.Remove(backingImageTmpPath); err != nil {
				return errors.Wrapf(err, "failed to delete tmp file %v before trying to reuse file %v", backingImageTmpPath, backingImagePath)
			}
		}
		if err := os.Rename(backingImagePath, backingImageTmpPath); err != nil {
			bi.log.WithError(err).Warnf("Backing Image: failed to rename existing file %v to tmp file %v before trying to reuse it, will fall back to clean up it", backingImagePath, backingImageTmpPath)
			if err := os.Remove(backingImagePath); err != nil {
				return errors.Wrapf(err, "failed to delete file %v before downloading", backingImagePath)
			}
		}
		return nil
	}

	return nil
}

func (bi *BackingImage) validateFiles() error {
	switch bi.state {
	case StateDownloading:
		backingImageTmpPath := filepath.Join(bi.WorkDirectory, types.BackingImageTmpFileName)
		if _, err := os.Stat(backingImageTmpPath); err != nil {
			return errors.Wrapf(err, "failed to validate backing image tmp file existence for downloading backing image")
		}
		return nil
	case StateDownloaded:
		backingImagePath := filepath.Join(bi.WorkDirectory, types.BackingImageFileName)
		if _, err := os.Stat(backingImagePath); err != nil {
			return errors.Wrapf(err, "failed to validate backing image file existence for downloaded backing image")
		}
		configFilePath := filepath.Join(bi.WorkDirectory, util.BackingImageConfigFile)
		if _, err := os.Stat(configFilePath); err != nil {
			return errors.Wrapf(err, "failed to validate backing image config file existence for downloaded backing image")
		}
	// Don't need to check anything for a failed/pending backing image.
	// Let's directly wait for cleanup then re-downloading.
	case StatePending:
	case StateFailed:
	default:
		return fmt.Errorf("unexpected state for file validation")
	}

	return nil
}

func (bi *BackingImage) completeDownloadWithLock(size int64) {
	backingImageTmpPath := filepath.Join(bi.WorkDirectory, types.BackingImageTmpFileName)
	backingImagePath := filepath.Join(bi.WorkDirectory, types.BackingImageFileName)

	bi.lock.Lock()
	defer bi.lock.Unlock()

	if bi.state == StateFailed {
		bi.log.Warnf("Backing Image: state somehow becomes %v after downloading, will not continue renaming file", types.DownloadStateFailed)
		return
	}

	var err error
	defer func() {
		if bi.downloadCanceller != nil {
			bi.downloadCanceller()
			bi.downloadCanceller = nil
		}
		if err != nil {
			bi.state = StateFailed
			bi.errorMsg = err.Error()
			bi.log.WithError(err).Error("Backing Image: failed to complete download")
		}
	}()

	if bi.processedSize != size {
		err = fmt.Errorf("processed size %v doesn't match written size %v", bi.processedSize, size)
		return
	}

	if err := os.Rename(backingImageTmpPath, backingImagePath); err != nil {
		err = errors.Wrapf(err, "failed to rename backing image file after downloading")
		return
	}

	if err := util.WriteBackingImageConfigFile(bi.WorkDirectory, &util.BackingImageConfig{
		Name: bi.Name,
		UUID: bi.UUID,
		URL:  bi.URL,
		Size: size,
	}); err != nil {
		err = errors.Wrapf(err, "failed to write backing image config file after downloading")
	}

	bi.size = size
	bi.progress = 100
	bi.state = StateDownloaded
	bi.log.Infof("Backing Image: downloaded backing image file")
	return
}

func (bi *BackingImage) UpdateSyncFileProgress(size int64) {
	bi.lock.Lock()
	defer bi.lock.Unlock()

	if bi.state == types.DownloadStatePending {
		bi.state = types.DownloadStateDownloading
	}

	bi.processedSize = bi.processedSize + size
	if bi.size > 0 {
		bi.progress = int((float32(bi.processedSize) / float32(bi.size)) * 100)
	}
}
