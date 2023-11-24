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

func (c *ClICtrl) syncEncFiles(ctx context.Context, account model.Account, param ExportParams) error {
	log.Printf("Starting encrypted download")
	albums, err := c.getRemoteAlbums(ctx)
	if err != nil {
		return err
	}
	// create thump dir if not exists
	thumbDir := filepath.Join(param.DevExport.Dir, "thumb")
	if _, err := os.Stat(thumbDir); os.IsNotExist(err) {
		if err = os.MkdirAll(thumbDir, os.ModePerm); err != nil {
			return err
		}
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

func (c *ClICtrl) processDownloads(ctx context.Context, entry model.RemoteFile, param ExportParams) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Recovered from panic: %v", r)
		}
	}()

	log.Printf("Downloading file %s", entry.GetTitle())
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
	// check if file exists
	if stat, err := os.Stat(downloadPath); err == nil {
		if stat.Size() == file.Info.FileSize {
			log.Printf("File already exists %s (%s)", file.GetTitle(), utils.ByteCountDecimal(file.Info.FileSize))
			return nil
		} else {
			log.Printf("File-%d: Exists  %s but size mismatch  remote: (%s) disk:(%s)",
				file.ID,
				file.GetTitle(), utils.ByteCountDecimal(file.Info.FileSize), utils.ByteCountDecimal(stat.Size()))
		}
	}
	log.Printf("Downloading %s (%s)", file.GetTitle(), utils.ByteCountDecimal(file.Info.FileSize))
	fastTempDownloadPath := fmt.Sprintf("%s/%d", c.tempFolder, file.ID)
	err := c.Client.DownloadFile(ctx, file.ID, fastTempDownloadPath)
	if err != nil {
		return fmt.Errorf("error downloading file %d: %w", file.ID, err)
	}
	err = moveCrossDevice(fastTempDownloadPath, downloadPath)
	if err != nil {
		return err
	}
	if !devExport.ShouldDecrypt {
		return nil
	}
	decryptedPath := fmt.Sprintf("%s/%d.decrypted", dir, file.ID)
	err = crypto.DecryptFile(downloadPath, decryptedPath, file.Key.MustDecrypt(deviceKey), encoding.DecodeBase64(file.FileNonce))
	if err != nil {
		log.Printf("Error decrypting file %d: %s", file.ID, err)
		return model.ErrDecryption
	} else {
		log.Printf("Decrypted file %s at %s", file.GetTitle(), decryptedPath)
		//_ = os.Remove(downloadPath)
		return err
	}

}

func (c *ClICtrl) downloadThumCache(ctx context.Context, file model.RemoteFile, dir string, deviceKey []byte, devExport *DevExport) error {
	dir = fmt.Sprintf("%s/thumb", dir)
	downloadPath := fmt.Sprintf("%s/%d", dir, file.ID)
	// check if file exists
	if stat, err := os.Stat(downloadPath); err == nil {
		if stat.Size() != file.Info.ThumbnailSize {
			log.Printf(" [Thumb-%d] Thumnail %s exists but size mismatch disk(%s) remote (%s)", file.ID, file.GetTitle(), utils.ByteCountDecimal(stat.Size()), utils.ByteCountDecimal(file.Info.ThumbnailSize))
		} else {
			return nil
		}
	}
	log.Printf("[Thumb-%d] Download Thumnail %s (%s)", file.ID, file.GetTitle(), utils.ByteCountDecimal(file.Info.ThumbnailSize))
	fastTempDownloadPath := fmt.Sprintf("%s/%d.thumb", c.tempFolder, file.ID)
	err := c.Client.DownloadThumb(ctx, file.ID, fastTempDownloadPath)
	if err != nil {
		return fmt.Errorf("error downloading thumbnail %d: %w", file.ID, err)
	}
	err = moveCrossDevice(fastTempDownloadPath, downloadPath)
	if err != nil {
		return err
	}
	if !devExport.ShouldDecrypt {
		return nil
	}
	decryptedPath := fmt.Sprintf("%s/%d.decrypted", dir, file.ID)
	err = crypto.DecryptFile(downloadPath, decryptedPath, file.Key.MustDecrypt(deviceKey), encoding.DecodeBase64(file.ThumbnailNonce))

	if err != nil {
		log.Printf("Error decrypting thumbnail file %d: %s", file.ID, err)
		return model.ErrDecryption
	} else {
		log.Printf("Decrypted Thumnail %s at %s", file.GetTitle(), decryptedPath)
		//_ = os.Remove(downloadPath)
		return err
	}

}
