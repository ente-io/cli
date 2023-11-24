package pkg

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/ente-io/cli/internal"
	"github.com/ente-io/cli/internal/api"
	"github.com/ente-io/cli/pkg/model"
	bolt "go.etcd.io/bbolt"
	"log"
	"time"
)

type DevExport struct {
	SkipVideo     bool
	MaxSizeInMB   int64
	ShouldDecrypt bool
	IncludeHidden bool
	Dir           string
	ParallelLimit int
	Email         string
}

type ExportParams struct {
	DevExport *DevExport
}

func (c *ClICtrl) Export(params *ExportParams) error {
	accounts, err := c.GetAccounts(context.Background())
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		fmt.Printf("No accounts to sync\n Add account using `account add` cmd\n")
		return nil
	}
	for _, account := range accounts {
		if account.App == api.AppAuth {
			log.Printf("Skip account %s: auth export is not supported", account.Email)
			continue
		}
		log.Println("start sync")
		retryCount := 0
		for {
			err = c.SyncAccount(account, params)
			if err != nil {
				if model.ShouldRetrySync(err) && retryCount < 20 {
					retryCount = retryCount + 1
					timeInSecond := time.Duration(retryCount*10) * time.Second
					log.Printf("Connection err, waiting for %s before trying again", timeInSecond.String())
					time.Sleep(timeInSecond)
					continue
				}
				fmt.Printf("Error syncing account %s: %s\n", account.Email, err)
				return err
			} else {
				log.Println("sync done")
				break
			}
		}

	}
	return nil
}

func (c *ClICtrl) SyncAccount(account model.Account, params *ExportParams) error {
	secretInfo, err := c.KeyHolder.LoadSecrets(account)
	if err != nil {
		return err
	}
	dirToValidate := account.ExportDir
	if params != nil && params.DevExport != nil && params.DevExport.Dir != "" {
		dirToValidate, _ = internal.ResolvePath(params.DevExport.Dir)
		if params.DevExport.Email != account.Email {
			log.Printf("Skip as email mismatch: %s != %s", params.DevExport.Email, account.Email)
			return nil
		}
	}
	log.SetPrefix(fmt.Sprintf("[%s-%s]", account.App, account.Email))
	if dirToValidate == "" {
		log.Printf("Skip account %s: no export directory configured", account.Email)
		return nil
	}
	_, err = internal.ValidateDirForWrite(dirToValidate)
	if err != nil {
		log.Printf("Skip export, error: %v while validing exportDir %s\n", err, dirToValidate)
		return nil
	}

	ctx := c.buildRequestContext(context.Background(), account)
	err = createDataBuckets(c.DB, account)
	if err != nil {
		return err
	}
	c.Client.AddToken(account.AccountKey(), base64.URLEncoding.EncodeToString(secretInfo.Token))
	err = c.fetchRemoteCollections(ctx)
	if err != nil {
		log.Printf("Error fetching collections: %s", err)
		return err
	}
	err = c.fetchRemoteFiles(ctx)
	if err != nil {
		log.Printf("Error fetching files: %s", err)
		return err
	}
	if params != nil && params.DevExport != nil && params.DevExport.Dir != "" {
		err = c.syncEncFiles(ctx, account, *params)
		if err != nil {
			log.Printf("Error downloading files: %s", err)
			return err
		}
	} else {
		err = c.createLocalFolderForRemoteAlbums(ctx, account)
		if err != nil {
			log.Printf("Error creating local folders: %s", err)
			return err
		}
		log.Printf("Syncing files for local server at %s", account.ExportDir)
		err = c.syncFiles(ctx, account)
		if err != nil {
			log.Printf("Error syncing files: %s", err)
			return err
		}
	}
	return nil
}

func (c *ClICtrl) buildRequestContext(ctx context.Context, account model.Account) context.Context {
	ctx = context.WithValue(ctx, "app", string(account.App))
	ctx = context.WithValue(ctx, "account_key", account.AccountKey())
	ctx = context.WithValue(ctx, "user_id", account.UserID)
	return ctx
}

func createDataBuckets(db *bolt.DB, account model.Account) error {
	return db.Update(func(tx *bolt.Tx) error {
		dataBucket, err := tx.CreateBucketIfNotExists([]byte(account.AccountKey()))
		if err != nil {
			return fmt.Errorf("create bucket: %s", err)
		}
		for _, subBucket := range []model.PhotosStore{model.KVConfig, model.RemoteAlbums, model.RemoteFiles, model.RemoteAlbumEntries} {
			_, err := dataBucket.CreateBucketIfNotExists([]byte(subBucket))
			if err != nil {
				return err
			}
		}
		return nil
	})
}
