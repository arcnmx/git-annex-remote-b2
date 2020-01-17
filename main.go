package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/arcnmx/go-git-annex-external/external"
	"github.com/kothar/go-backblaze"
)

type B2Ext struct {
	bucket *backblaze.Bucket
	prefix string
	retries int

	cache struct {
		filemap     map[string]string
		enabled     bool
		incomplete  bool
		duration    time.Duration
		timeWritten time.Time
	}

	lastList struct {
		setAt time.Time
		file  string
		found bool
		id    string
	}
}

type configValues struct {
	accountID string
	appKey string
	keyID string
	bucketName string
	prefix string
	retryCount string
	cacheFilenames string
	cacheFilesnamesDuration string
}

func authenticate(e *external.External, accountID string, appKey string, keyID string) (*backblaze.B2, error) {
	b2, err := backblaze.NewB2(backblaze.Credentials{
		AccountID:      accountID,
		ApplicationKey: appKey,
		KeyID:          keyID,
	})
	if err != nil {
		return nil, fmt.Errorf("Couldn't authorize: %v", err)
	}

	return b2, nil
}

func openBucket(b2 *backblaze.B2, bucketName string, canCreateBucket bool) (*backblaze.Bucket, error) {
	bucket, err := b2.Bucket(bucketName)
	if err != nil {
		return nil, fmt.Errorf("couldn't open bucket %#v: %v", bucketName, err)
	}

	if bucket == nil {
		if !canCreateBucket {
			return nil, fmt.Errorf("bucket %#v does not exist anymore", bucketName)
		}

		fmt.Fprintf(os.Stderr, "Creating private B2 bucket %#v\n", bucketName)

		bucket, err = b2.CreateBucket(bucketName, backblaze.AllPrivate)
		if err != nil {
			return nil, fmt.Errorf("couldn't create bucket %#v: %v", bucketName, err)
		}
	}

	return bucket, err
}

func getConfig(e *external.External) (config configValues, err error) {
	config = configValues{}

	bucketCred := ""
	hasBucketCred := false
	config.accountID, err = os.Getenv("B2_ACCOUNT_ID"), nil
	if config.accountID == "" {
		config.accountID, err = e.GetConfig("accountid")
	}
	if config.accountID == "" && err == nil {
		config.accountID, bucketCred, err = e.GetCreds("b2_account")
		hasBucketCred = true
	}
	if err != nil {
		return
	}
	if config.accountID == "" {
		err = errors.New("You must set B2_ACCOUNT_ID to the backblaze account id")
		return
	}

	config.keyID, config.appKey = os.Getenv("B2_KEY_ID"), os.Getenv("B2_APP_KEY")

	if config.appKey == "" {
		config.appKey, err = e.GetConfig("appkey")
		if err == nil {
			config.keyID, err = e.GetConfig("appkeyid")
		}
	}
	if config.appKey == "" && err == nil {
		config.keyID, config.appKey, err = e.GetCreds("b2_appkey")
	}
	if err != nil {
		return
	}
	if config.appKey == "" {
		err = errors.New("You must set B2_APP_KEY to the backblaze application key")
		return
	}

	config.bucketName = os.Getenv("B2_BUCKET")
	if config.bucketName == "" {
		config.bucketName, err = e.GetConfig("bucket")
	}
	if config.bucketName == "" && err == nil {
		if !hasBucketCred {
			_, bucketCred, err = e.GetCreds("b2_account")
		}
		if err == nil {
			config.bucketName = bucketCred
		}
	}
	if err != nil {
		return
	}
	if config.bucketName == "" {
		err = errors.New("You must set bucket to the bucket name")
		return
	}

	config.prefix, err = e.GetConfig("prefix")
	// prefix == "" is ok.
	if config.prefix != "" && !strings.HasSuffix(config.prefix, "/") {
		config.prefix = config.prefix + "/"
	}

	config.retryCount = os.Getenv("B2_RETRY_COUNT")
	if config.retryCount == "" {
		config.retryCount, err = e.GetConfig("retry-count")
	}
	if err != nil {
		return
	}

	config.cacheFilenames = os.Getenv("B2_CACHE_FILENAMES")
	if config.cacheFilenames == "" {
		config.cacheFilenames, err = e.GetConfig("cache-filenames")
	}
	if err != nil {
		return
	}

	config.cacheFilesnamesDuration = os.Getenv("B2_CACHE_FILENAMES_DURATION")
	if config.cacheFilesnamesDuration == "" {
		config.cacheFilesnamesDuration, err = e.GetConfig("cache-filenames-duration")
	}
	if err != nil {
		return
	}

	return
}

func (be *B2Ext) initFileMap() (err error) {
	be.cache.filemap = make(map[string]string)
	nextfile := ""
	for i := 0; i < 100; i++ {
		response, err := be.bucket.ListFileNames(nextfile, 10000)
		if err != nil {
			return err
		}
		for _, file := range response.Files {
			be.cache.filemap[file.Name] = file.ID
		}
		nextfile = response.NextFileName
		if nextfile == "" {
			break
		}
	}
	be.cache.timeWritten = time.Now()
	if nextfile != "" {
		be.cache.incomplete = true
	}
	return nil
}

func (be *B2Ext) listFileCached(file string) (found bool, fileID string, err error) {
	if be.cache.enabled {
		if be.cache.filemap == nil || be.cache.duration != 0 && time.Since(be.cache.timeWritten) > be.cache.duration {
			err = be.initFileMap()
			if err != nil {
				be.cache.filemap = nil
				return false, "", err
			}
		}

		if be.cache.filemap[file] != "" {
			return true, be.cache.filemap[file], nil
		}
		if !be.cache.incomplete {
			return false, "", nil
		}
	}

	// Caching the last result of ListFileNames is no less safe than not caching
	// it; the race condition of two concurrent git annex copy --to b2 processes
	// sending the same file can result in a file with two identical versions in
	// both cases.
	//
	// However, caching this reduces the number of ListFileNames to half of what
	// it is during uploads (since git-annex always calls checkpresent which
	// uses ListFileNames before uploading, but when uploading we also do
	// upload elision by calling ListFileNames.)

	if be.lastList.file != file || time.Since(be.lastList.setAt) > time.Second*15 {
		res, err := be.bucket.ListFileNames(file, 1)
		if err != nil {
			return false, "", err
		}

		be.lastList.setAt = time.Now()
		if len(res.Files) == 0 || res.Files[0].Name != file || res.Files[0].Action != backblaze.Upload {
			be.lastList.file = file
			be.lastList.found = false
			be.lastList.id = ""
		} else {
			be.lastList.file = file
			be.lastList.found = true
			be.lastList.id = res.Files[0].ID
		}
	}

	return be.lastList.found, be.lastList.id, nil
}

func (be *B2Ext) clearListFileCache() {
	be.lastList.setAt = time.Time{}
	be.lastList.file = ""
	be.lastList.found = false
	be.lastList.id = ""
}

func (be *B2Ext) setup(e *external.External, canCreateBucket bool) error {
	if be.bucket != nil {
		// already done!
		return nil
	}

	config, err := getConfig(e)
	if err != nil {
		return err
	}

	s := config.retryCount
	if s == "" {
		be.retries = 1
	} else {
		n, err := strconv.Atoi(s)
		if err != nil {
			return err
		} else {
			be.retries = n
		}
	}

	s = config.cacheFilenames
	if s == "" {
		be.cache.enabled = false
	} else {
		be.cache.enabled, err = strconv.ParseBool(s)
		if err != nil {
			return err
		}
	}

	s = config.cacheFilesnamesDuration
	if s == "" {
		be.cache.duration = time.Duration(0)
	} else {
		n, err := strconv.Atoi(s)
		if err == nil {
			be.cache.duration = time.Duration(n) * time.Second
		} else {
			be.cache.duration, err = time.ParseDuration(s)
			if err != nil {
				return err
			}
		}
	}
	if be.cache.duration < 0 {
		return errors.New("cache duration must be non-negative")
	}

	b2, err := authenticate(e, config.accountID, config.appKey, config.keyID)
	if err != nil {
		return err
	}

	bucket, err := openBucket(b2, config.bucketName, canCreateBucket)
	if err != nil {
		return err
	}

	be.bucket = bucket
	be.prefix = config.prefix

	if canCreateBucket {
		err = e.SetCreds("b2_account", config.accountID, config.bucketName)
		if err != nil {
			return err
		}

		err = e.SetCreds("b2_appkey", config.keyID, config.appKey)
		if err != nil {
			return err
		}
	}

	return nil
}

func (be *B2Ext) InitRemote(e *external.External) error {
	return be.setup(e, true)
}

func (be *B2Ext) Prepare(e *external.External) error {
	return be.setup(e, false)
}

func (be *B2Ext) Store(e *external.External, key, file string) error {
	fh, err := os.Open(file)
	if err != nil {
		return err
	}
	defer fh.Close()

	shaReady := make(chan struct{})
	var haveSHA []byte
	var contentLength int64
	var shaError error
	go func() {
		defer close(shaReady)

		sha := sha1.New()
		contentLength, shaError = io.Copy(sha, fh)
		if shaError != nil {
			return
		}

		haveSHA = sha.Sum(nil)

		_, shaError = fh.Seek(0, 0)
	}()

	found, fileID, err := be.listFileCached(be.prefix + key)
	if err != nil {
		return fmt.Errorf("couldn't list filenames: %v", err)
	}

	if found {
		// file probably already stored; make sure using the SHA1
		b2file, err := be.bucket.GetFileInfo(fileID)
		if err != nil {
			return fmt.Errorf("couldn't get file info for %#v: %v", fileID, err)
		}
		if b2file != nil {
			<-shaReady

			wantSHA, err := hex.DecodeString(b2file.ContentSha1)
			if err == nil && bytes.Equal(haveSHA, wantSHA) {
				// File already exists with correct data.
				return nil
			}
		}
	}

	<-shaReady
	if shaError != nil {
		return fmt.Errorf("couldn't hash local file %v: %v", file, shaError)
	}

	for i := uint(0); i < uint(be.retries + 1); i++ {
		b2file, err := be.bucket.UploadHashedFile(
			be.prefix+key,
			nil,
			external.NewProgressReader(fh, e),
			hex.EncodeToString(haveSHA),
			contentLength)

		if b2err, ok := err.(*backblaze.B2Error); ok {
			if b2err.IsFatal() {
				return fmt.Errorf("couldn't upload file: %v", err)
			} else {
				wait := time.Duration(1 << i) * time.Second
				e.Debug(fmt.Sprintf("upload failed, retrying in %v, error: %v", wait, err))

				_, err = fh.Seek(0, 0)
				if err != nil {
					return fmt.Errorf("couldn't retry upload: %v", err)
				}

				time.Sleep(wait)
			}
		} else if err != nil {
			return fmt.Errorf("couldn't upload file: %v", err)
		} else {
			be.clearListFileCache()
			if be.cache.enabled {
				be.cache.filemap[b2file.Name] = b2file.ID
			}
			break
		}
	}

	return nil
}

func (be *B2Ext) Retrieve(e *external.External, key, file string) error {
	fh, err := os.Create(file)
	if err != nil {
		return fmt.Errorf("couldn't open %v for writing: %v", file, err)
	}
	defer fh.Close()

	_, rc, err := be.bucket.DownloadFileByName(be.prefix + key)
	if rc != nil {
		defer rc.Close()
	}
	if err != nil {
		return err
	}

	_, err = io.Copy(fh, external.NewProgressReader(rc, e))
	if err != nil {
		return err
	}

	return nil
}

func (be *B2Ext) CheckPresent(e *external.External, key string) (bool, error) {
	found, _, err := be.listFileCached(be.prefix + key)
	if err != nil {
		return false, fmt.Errorf("couldn't list filenames: %v", err)
	}

	return found, nil
}

func (be *B2Ext) Remove(e *external.External, key string) error {
	found, _, err := be.listFileCached(be.prefix + key)
	if err != nil {
		return fmt.Errorf("couldn't list filenames: %v", err)
	}

	if !found {
		// File already non-existent, nothing to remove
		return nil
	}

	_, err = be.bucket.HideFile(be.prefix + key)
	be.clearListFileCache()
	if err != nil {
		return fmt.Errorf("couldn't delete file version: %v", err)
	}

	return nil
}

func (be *B2Ext) GetCost(e *external.External) (int, error) {
	return 0, external.ErrUnsupportedRequest
}

func (be *B2Ext) GetAvailability(e *external.External) (external.Availability, error) {
	return external.AvailabilityGlobal, nil
}

func (be *B2Ext) WhereIs(e *external.External, key string) (string, error) {
	if be.bucket.BucketType == backblaze.AllPublic {
		// this generally shouldn't touch the network but might if auth is invalidated :(
		return be.bucket.FileURL(be.prefix + key)
	} else {
		return "", nil
	}
}

func main() {
	h := &B2Ext{}

	var (
		in  io.Reader = os.Stdin
		out io.Writer = os.Stdout
	)

	if os.Getenv("GIT_ANNEX_EXTERNAL_B2_PROTOCOL_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "git-annex-remote-b2: enabling protocol debug logging\n")
		in = io.TeeReader(in, os.Stderr)
		out = io.MultiWriter(out, os.Stderr)
	}

	err := external.RunLoop(in, out, h)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	os.Exit(0)
}
