package pkg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ente-io/cli/internal/crypto"
	"github.com/ente-io/cli/pkg/model"
	"github.com/ente-io/cli/utils"
	"github.com/ente-io/cli/utils/encoding"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

type dstType string

const (
	Encrypted dstType = "encrypted"
	Decrypted dstType = "decrypted"
	Download  dstType = "download"
	FileJSON  dstType = "fileJson"
)

const (
	ThumbDir           = "thumb"
	DecryptedDir       = "decrypted"
	PreviousVersionDir = "decrypted/updated"
)

func (c *ClICtrl) syncEncFiles(ctx context.Context, account model.Account, param ExportParams) error {
	log.Printf("Starting encrypted download")
	albums, err := c.getRemoteAlbums(ctx)
	if err != nil {
		return err
	}
	err2 := _createExportSubDirectories(param)
	if err2 != nil {
		return err2
	}
	deletedAlbums := make(map[int64]bool)
	for _, album := range albums {
		if album.IsDeleted {
			deletedAlbums[album.ID] = true
		}
	}
	entries, err := c.getRemoteAlbumEntries(ctx)
	if err != nil {
		return err
	}
	log.Println("total entries", len(entries))
	model.SortAlbumFileEntry(entries)
	defer utils.TimeTrack(time.Now(), "process_files")

	var wg sync.WaitGroup
	var channelSize = 1
	workers := param.DevExport.ParallelLimit
	log.Printf("Setting maxProcs %d, worker count %d", runtime.GOMAXPROCS(param.DevExport.ParallelLimit), workers)
	downloadCh := make(chan model.RemoteFile, channelSize) // Limit the number of parallel downloads to 5
	// Start worker goroutines
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for entry := range downloadCh {
				c.processDownloads(ctx, entry, param)
			}
		}()
	}

	queuedFiles := make(map[int64]bool)

	for i, entry := range entries {
		if entry.IsDeleted {
			continue
		}
		if _, ok := deletedAlbums[entry.AlbumID]; ok {
			continue
		}
		if _, ok := queuedFiles[entry.FileID]; ok {
			continue
		}
		queuedFiles[entry.FileID] = true
		fileBytes, err := c.GetValue(ctx, model.RemoteFiles, []byte(fmt.Sprintf("%d", entry.FileID)))
		if err != nil {
			return err
		}

		if fileBytes != nil {
			var existingEntry *model.RemoteFile
			err = json.Unmarshal(fileBytes, &existingEntry)
			if err != nil {
				return err
			}
			log.Printf("Progress [%d/%d] ", i+1, len(entries))
			// Add the entry to the download channel
			downloadCh <- *existingEntry
		} else {
			log.Printf("remoteFile %d not found in remoteFiles", entry.FileID)
		}
	}

	// Close the download channel to signal workers to stop
	close(downloadCh)
	wg.Wait()
	return nil
}

func _createExportSubDirectories(param ExportParams) error {
	// create thump dir if not exists
	thumbDir := filepath.Join(param.DevExport.Dir, ThumbDir)
	if _, err := os.Stat(thumbDir); os.IsNotExist(err) {
		if err = os.MkdirAll(thumbDir, os.ModePerm); err != nil {
			return err
		}
	}
	if param.DevExport.ShouldDecrypt {
		decryptFileFOlder := filepath.Join(param.DevExport.Dir, DecryptedDir)
		if _, err := os.Stat(decryptFileFOlder); os.IsNotExist(err) {
			if err = os.MkdirAll(decryptFileFOlder, os.ModePerm); err != nil {
				return err
			}
		}
		decryptedThumbDir := filepath.Join(decryptFileFOlder, ThumbDir)
		if _, err := os.Stat(decryptedThumbDir); os.IsNotExist(err) {
			if err = os.MkdirAll(decryptedThumbDir, os.ModePerm); err != nil {
				return err
			}
		}
		// use same directory for both file and thumb
		oldFileVersionDir := filepath.Join(param.DevExport.Dir, PreviousVersionDir)
		if _, err := os.Stat(oldFileVersionDir); os.IsNotExist(err) {
			if err = os.MkdirAll(oldFileVersionDir, os.ModePerm); err != nil {
				return err
			}
		}

	}
	return nil
}

func (c *ClICtrl) getDstPath(file model.RemoteFile,
	isThumbnail bool,
	dType dstType) string {
	switch dType {
	case Encrypted:
		if isThumbnail {
			return fmt.Sprintf("%s/%d", ThumbDir, file.ID)
		}
		return fmt.Sprintf("%d", file.ID)
	case Decrypted:
		if isThumbnail {
			return fmt.Sprintf("%s/%s/%d", DecryptedDir, ThumbDir, file.ID)
		}
		return fmt.Sprintf("%s/%d", DecryptedDir, file.ID)
	case FileJSON:
		return fmt.Sprintf("%s/%d.json", DecryptedDir, file.ID)
	case Download:
		if isThumbnail {
			return fmt.Sprintf("%s/%d.thumb", c.tempFolder, file.ID)
		}
		return fmt.Sprintf("%s/%d", c.tempFolder, file.ID)
	}
	panic("invalid dstType")
}

// getTargetForPreviousVersion returns the target file prefix where the previous version of the file should be stored
func getTargetForPreviousVersion(dir string, file model.RemoteFile) string {
	startVer := 0
	for {
		targetDir := fmt.Sprintf("%s/%d_%d", PreviousVersionDir, file.ID, startVer)
		// check if file exists
		if _, err := os.Stat(filepath.Join(dir, targetDir+".json")); err == nil {
			startVer++
		} else {
			log.Printf(fmt.Sprintf("File %s not found", dir+targetDir+".json"))
			return targetDir

		}
		if startVer > 10 {
			panic(fmt.Sprintf("too many versions for file %d, title %s", file.ID, file.GetTitle()))
		}
	}
	panic("should not reach here")
}

func (c *ClICtrl) processDownloads(ctx context.Context, entry model.RemoteFile, param ExportParams) {
	//log.Printf("Progress [file:%d] %s", entry.ID, entry.GetTitle())
	err := c.downloadEncrypted(ctx, entry, param)
	if err != nil {
		if errors.Is(err, model.ErrDecryption) {
			log.Printf("file[%d]: Error decrypting file %s: %s", entry.ID, entry.GetTitle(), err)
		} else {
			log.Printf("file[%d]: Error downloading file %s: %s", entry.ID, entry.GetTitle(), err)
			// Handle the error as needed
		}
	}
}

func (c *ClICtrl) downloadEncrypted(ctx context.Context,
	file model.RemoteFile,
	params ExportParams,
) error {
	err := c.downloadThumCache(ctx, file, params.DevExport.Dir, c.KeyHolder.DeviceKey, params.DevExport)
	if err != nil {
		return err
	}
	if file.GetFileType() == model.Video && params.DevExport.SkipVideo {
		log.Printf("Skipping original file, just download thumb video %s", file.GetTitle())
		return nil
	}
	if params.DevExport.MaxSizeInMB > 0 {
		if file.Info.FileSize > (params.DevExport.MaxSizeInMB * 1024 * 1024) {
			log.Printf("File-%d: %s too large , skipped.. size: %s", file.ID, file.GetTitle(), utils.ByteCountDecimal(file.Info.FileSize))
			return nil
		}
	}
	return c.downloadCache(ctx, file, params.DevExport.Dir, c.KeyHolder.DeviceKey, params.DevExport)
}

func (c *ClICtrl) downloadCache(ctx context.Context, file model.RemoteFile, dir string, deviceKey []byte, devExport *DevExport) error {
	downloadPath := fmt.Sprintf("%s/%d", dir, file.ID)
	decryptedPath := filepath.Join(dir, c.getDstPath(file, false, Decrypted))
	jsonPath := filepath.Join(dir, c.getDstPath(file, false, FileJSON))
	identifier := fmt.Sprintf("[File:%d] %s", file.ID, file.GetTitle())
	remoteSize := file.Info.FileSize
	alreadyDownloaded := false
	if stat, err := os.Stat(downloadPath); err == nil {
		if stat.Size() == remoteSize {
			alreadyDownloaded = true
			if devExport.ShouldDecrypt {
				if _, err := os.Stat(decryptedPath); err == nil {
					//log.Printf("%s already  existis and decrypted", identifier)
					return nil
				} else {
					log.Printf("%s exists but not decrypted", identifier)
				}
			} else {
				log.Printf("%s already exists(%s)", identifier, utils.ByteCountDecimal(remoteSize))
				return nil
			}

		} else {
			log.Printf("%s exists but size mismatch  remote: (%s) disk:(%s)", identifier, utils.ByteCountDecimal(remoteSize), utils.ByteCountDecimal(stat.Size()))
		}
	}
	if !alreadyDownloaded {
		log.Printf("%s downloading (%s)", identifier, utils.ByteCountDecimal(remoteSize))
		fastTempDownloadPath := fmt.Sprintf("%s/%d", c.tempFolder, file.ID)
		err := c.Client.DownloadFile(ctx, file.ID, fastTempDownloadPath)
		if err != nil {
			return fmt.Errorf("error downloading file %d: %w", file.ID, err)
		}
		if !devExport.ShouldDecrypt {
			go moveCrossDevice(fastTempDownloadPath, downloadPath)
			return nil
		}
		err = moveCrossDevice(fastTempDownloadPath, downloadPath)
		if err != nil {
			return err
		}
	}

	// check if file exists
	if _, err := os.Stat(decryptedPath); err == nil {
		// returns path like dir/decrypted/updated/{fileID}_version
		previousVerPath := filepath.Join(dir, getTargetForPreviousVersion(dir, file))
		extension := filepath.Ext(file.GetTitle())
		if file.GetFileType() == model.LivePhoto {
			extension = "live_" + extension
		}
		err = moveCrossDevice(decryptedPath, fmt.Sprintf("%s%s", previousVerPath, extension))
		if err != nil {
			return fmt.Errorf("error moving file %s: %s", decryptedPath, err)
		}
		if _, errr := os.Stat(jsonPath); errr == nil {
			previousVerJsonPath := fmt.Sprintf("%s.json", previousVerPath)
			err = moveCrossDevice(jsonPath, previousVerJsonPath)
			if err != nil {
				return fmt.Errorf("error moving json file %s: %s", jsonPath, err)
			}
		}
	}
	err := crypto.DecryptFile(downloadPath, decryptedPath, file.Key.MustDecrypt(deviceKey), encoding.DecodeBase64(file.FileNonce))
	if err != nil {
		log.Printf("%s error decrypting  %s", identifier, err)
		return model.ErrDecryption
	}

	err = writeJSONToFile(jsonPath, file)
	if err != nil {
		return fmt.Errorf("error writing json file %s: %w", jsonPath, err)
	}
	log.Printf("%s decrypted at %s", identifier, decryptedPath)
	//_ = os.Remove(downloadPath)
	return err

}

func (c *ClICtrl) downloadThumCache(ctx context.Context, file model.RemoteFile, dir string, deviceKey []byte, devExport *DevExport) error {
	encThumbPath := filepath.Join(dir, c.getDstPath(file, true, Encrypted))
	decryptedPath := filepath.Join(dir, c.getDstPath(file, true, Decrypted))
	alreadyDownloaded := false
	// check if file exists
	identifier := fmt.Sprintf("[Thum:%d] %s", file.ID, file.GetTitle())
	remoteSize := file.Info.ThumbnailSize
	if stat, err := os.Stat(encThumbPath); err == nil {
		if stat.Size() == remoteSize {
			alreadyDownloaded = true
			if devExport.ShouldDecrypt {
				if _, err := os.Stat(decryptedPath); err == nil {
					//log.Printf("%s already  existis and decrypted", identifier)
					return nil
				} else {
					log.Printf("%s exists but not decrypted", identifier)
				}
			} else {
				log.Printf("%s already exists(%s)", identifier, utils.ByteCountDecimal(remoteSize))
				return nil
			}
		} else {
			log.Printf("%s exists but size mismatch  remote: (%s) disk:(%s)", identifier, utils.ByteCountDecimal(remoteSize), utils.ByteCountDecimal(stat.Size()))
		}
	}
	if !alreadyDownloaded {
		log.Printf("%s downloading %s", identifier, utils.ByteCountDecimal(remoteSize))
		fastTempDownloadPath := fmt.Sprintf("%s/%d.thumb", c.tempFolder, file.ID)
		err := c.Client.DownloadThumb(ctx, file.ID, fastTempDownloadPath)
		if err != nil {
			return fmt.Errorf("error downloading thumbnail %d: %w", file.ID, err)
		}
		if !devExport.ShouldDecrypt {
			go moveCrossDevice(fastTempDownloadPath, encThumbPath)
			return nil
		}
		err = moveCrossDevice(fastTempDownloadPath, encThumbPath)
		if err != nil {
			return err
		}
	}
	if _, err := os.Stat(decryptedPath); err == nil {
		previousVerPath := filepath.Join(dir, getTargetForPreviousVersion(dir, file)) + "_thumb.jpg"
		err = moveCrossDevice(decryptedPath, previousVerPath)
		if err != nil {
			return fmt.Errorf("error moving file %s: %s", decryptedPath, err)
		}
	}
	err := crypto.DecryptFile(encThumbPath, decryptedPath, file.Key.MustDecrypt(deviceKey), encoding.DecodeBase64(file.ThumbnailNonce))
	if err != nil {
		log.Printf("%s Error decrypting %d ", identifier, err)
		return model.ErrDecryption
	} else {
		log.Printf("%s decrypted  at %s", identifier, decryptedPath)
		//_ = os.Remove(downloadPath)
		return err
	}

}
