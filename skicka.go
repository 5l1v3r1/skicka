//
// skicka.go
// Copyright(c)2014 Google, Inc.
//
// Tool for transferring files to/from Google Drive and related operations.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package main

import (
	"bytes"
	"code.google.com/p/gcfg"
	"code.google.com/p/go.crypto/pbkdf2"
	"code.google.com/p/goauth2/oauth"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"github.com/cheggaaa/pb"
	"google.golang.org/api/drive/v2"
	"google.golang.org/api/googleapi"
	"io"
	"io/ioutil"
	"log"
	mathrand "math/rand"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const timeFormat = "2006-01-02T15:04:05.000000000Z07:00"
const encryptionSuffix = ".aes256"
const resumableUploadMinSize = 64 * 1024 * 1024

///////////////////////////////////////////////////////////////////////////
// Global Variables

type debugging bool

var (
	oAuthTransport *oauth.Transport
	drivesvc       *drive.Service

	// The key is only set if encryption is needed (i.e. if -encrypt is
	// provided for an upload, or if an encrypted file is encountered
	// during 'download' or 'cat').
	key []byte

	debug   debugging
	verbose debugging

	// Configuration read in from the skicka config file.
	config struct {
		Google struct {
			ClientId     string
			ClientSecret string
		}
		Encryption struct {
			Salt             string
			Passphrase_hash  string
			Encrypted_key    string
			Encrypted_key_iv string
		}
		Upload struct {
			Ignored_Regexp         []string
			Bytes_per_second_limit int
		}
		Download struct {
			Bytes_per_second_limit int
		}
	}

	// Various statistics gathered along the way. These all should be
	// updated using atomic operations since we generally have multiple
	// threads working concurrently for uploads and downloads.
	stats struct {
		DiskReadBytes     int64
		DiskWriteBytes    int64
		UploadBytes       int64
		DownloadBytes     int64
		LocalFilesUpdated int64
		DriveFilesUpdated int64
	}

	passphraseEnvironmentVariable = "SKICKA_PASSPHRASE"
)

///////////////////////////////////////////////////////////////////////////
// Utility types

func (d debugging) Printf(format string, args ...interface{}) {
	if d {
		log.Printf(format, args...)
	}
}

// RetryHTTPTransmitError is a small struct to let us detect error cases
// where the caller should retry the operation, as the error seems to be a
// transient HTTP issue.
type RetryHTTPTransmitError struct {
	StatusCode int
	StatusBody string
}

func (r RetryHTTPTransmitError) Error() string {
	return fmt.Sprintf("http %d error (%s); retry", r.StatusCode, r.StatusBody)
}

type CommandSyntaxError struct {
	Cmd string
	Msg string
}

func (c CommandSyntaxError) Error() string {
	return fmt.Sprintf("%s syntax error: %s", c.Cmd, c.Msg)
}

// FileCloser is kind of a hack: it implements the io.ReadCloser
// interface, wherein the Read() calls go to R, and the Close() call
// goes to C.
type FileCloser struct {
	R io.Reader
	C *os.File
}

func (fc *FileCloser) Read(b []byte) (int, error) {
	return fc.R.Read(b)
}

func (fc *FileCloser) Close() error {
	return fc.C.Close()
}

// SomewhatSeekableReader is an io.Reader that can seek backwards from the
// current offset up to 'bufSize' bytes. It's useful for chunked file
// uploads, where we may need to rewind a bit after a failed chunk, but
// definitely don't want to pay the overhead of having the entire file in
// memory.
//
// It is implemented as a ring-buffer: the current offset in buf to read
// from is in readOffset, and the currentOffset to copy values read from
// the reader to is in writeOffset.  Both of these are taken mod bufSize
// when used to compute offsets into buf.
type SomewhatSeekableReader struct {
	R                       io.Reader
	buf                     []byte
	bufSize                 int
	readOffset, writeOffset int64
}

func MakeSomewhatSeekableReader(r io.Reader, size int) *SomewhatSeekableReader {
	return &SomewhatSeekableReader{
		R:           r,
		buf:         make([]byte, size),
		bufSize:     size,
		readOffset:  0,
		writeOffset: 0,
	}
}

func (ssr *SomewhatSeekableReader) Read(b []byte) (int, error) {
	// If the caller has called Seek() to move backwards from the
	// current read point of the underlying reader R, we start by
	// copying values from our local buffer into the output buffer.
	nCopied := 0
	if ssr.readOffset < ssr.writeOffset {
		for ; ssr.readOffset < ssr.writeOffset && nCopied < len(b); nCopied++ {
			b[nCopied] = ssr.buf[ssr.readOffset%int64(ssr.bufSize)]
			ssr.readOffset++
		}
		if nCopied == len(b) {
			return nCopied, nil
		}
	}

	// Once we're through the values we have locally buffered, we read
	// from the underlying reader. Note that we read into b[] starting
	// at the point where we stopped copying local values.
	nRead, err := ssr.R.Read(b[nCopied:])

	// Now update our local buffer of read values.  Note that this loop
	// is a bit wasteful in the case where nRead > ssr.bufSize; some of
	// the values it writes will be clobbered by a later iteration of
	// the loop.  (It's not clear that this is a big enough issue to
	// really worry about.)
	for i := 0; i < nRead; i++ {
		ssr.buf[ssr.writeOffset%int64(ssr.bufSize)] = b[nCopied+i]
		ssr.readOffset++
		ssr.writeOffset++
	}

	return nCopied + nRead, err
}

func (ssr *SomewhatSeekableReader) SeekTo(offset int64) error {
	if offset > ssr.writeOffset {
		panic("invalid seek")
	}
	if ssr.writeOffset-offset > int64(ssr.bufSize) {
		return fmt.Errorf("can't seek back to %d; current offset %d",
			offset, ssr.writeOffset)
	}
	ssr.readOffset = offset
	return nil
}

///////////////////////////////////////////////////////////////////////////
// Bandwidth-limiting io.Reader

// Maximum number of bytes of data that we are currently allowed to
// upload or download given the bandwidth limits set by the user, if any.
// This value is reduced by the RateLimitedReader.Read() method when data is
// uploaded or downloaded, and is periodically increased by the task
// launched by launchBandwidthTask().
var bandwidthBudget int

// Mutex to protect bandwidthBudget.
var bandwidthBudgetMutex sync.Mutex

func launchBandwidthTask(bytesPerSecond int) {
	if bytesPerSecond == 0 {
		// No limit, so no need to launch the task.
		return
	}

	go func() {
		for {
			bandwidthBudgetMutex.Lock()

			// Release 1/8th of the per-second limit every 8th of a second.
			// The 92/100 factor in the amount released adds some slop to
			// account for TCP/IP overhead in an effort to have the actual
			// bandwidth used not exceed the desired limit.
			bandwidthBudget += bytesPerSecond * 92 / 100 / 8
			if bandwidthBudget > bytesPerSecond {
				bandwidthBudget = bytesPerSecond
			}

			bandwidthBudgetMutex.Unlock()
			time.Sleep(time.Duration(125) * time.Millisecond)
		}
	}()
}

// RateLimitedReader is an io.Reader implementation that returns no more bytes
// than the current value of bandwidthBudget.  Thus, as long as the upload and
// download paths wrap the underlying io.Readers for local files and GETs
// from Drive (respectively), then we should stay under the bandwidth per
// second limit.
type RateLimitedReader struct {
	R io.Reader
}

func (lr RateLimitedReader) Read(dst []byte) (int, error) {
	// Loop until some amount of bandwidth is available.
	for {
		bandwidthBudgetMutex.Lock()
		if bandwidthBudget < 0 {
			panic("bandwidth budget went negative")
		}
		if bandwidthBudget > 0 {
			break
		}

		// No further uploading is possible at the moment;
		// sleep for a bit and then we'll try the loop
		// again and see if we have better luck...
		// TODO: we could also wait on a condition
		// variable and wait to be signaled by the "add
		// more available upload bytes" thread.
		bandwidthBudgetMutex.Unlock()
		time.Sleep(time.Duration(100) * time.Millisecond)
	}

	// The caller would like us to return up to this many bytes...
	n := len(dst)

	// but don't try to upload more than we're allowed to...
	if n > bandwidthBudget {
		n = bandwidthBudget
	}

	// Update the budget for the maximum amount of what we may consume and
	// relinquish the lock so that other workers can claim bandwidth.
	bandwidthBudget -= n
	bandwidthBudgetMutex.Unlock()

	read, err := lr.R.Read(dst[:n])
	if read < n {
		// It may turn out that the amount we read from the original
		// io.Reader is less than the caller asked for; in this case,
		// we give back the bandwidth that we reserved but didn't use.
		bandwidthBudgetMutex.Lock()
		bandwidthBudget += n - read
		bandwidthBudgetMutex.Unlock()
	}

	return read, err
}

type ByteCountingReader struct {
	R         io.Reader
	bytesRead int
}

func (bcr *ByteCountingReader) Read(dst []byte) (int, error) {
	read, err := bcr.R.Read(dst)
	bcr.bytesRead += read
	return read, err
}

///////////////////////////////////////////////////////////////////////////
// Small utility functions

var lastTimeDelta = time.Now()

// If debugging output is enabled, prints the elapsed time between the last
// call to timeDelta() (or program start, if it hasn't been called before),
// and the current call to timeDelta().
func timeDelta(event string) {
	now := time.Now()
	debug.Printf("Time [%s]: %s", event, now.Sub(lastTimeDelta).String())
	lastTimeDelta = now
}

// If the given path starts with a tilde, performs shell glob expansion
// to convert it to the path of the home directory. Otherwise returns the
// path unchanged.
func tildeExpand(path string) (string, error) {
	path = filepath.Clean(path)
	if path[:2] == "~/" {
		usr, err := user.Current()
		if err != nil {
			return path, err
		}
		homedir := usr.HomeDir
		return strings.Replace(path, "~", homedir, 1), nil
	} else if path[:1] == "~" {
		slashindex := strings.Index(path, "/")
		var username string
		if slashindex == -1 {
			username = path[1:]
		} else {
			username = path[1:slashindex]
		}
		usr, err := user.Lookup(username)
		if err != nil {
			return path, err
		}
		homedir := usr.HomeDir
		return homedir + path[slashindex:], nil
	} else {
		return path, nil
	}
}

// Utility function to decode hex-encoded bytes; treats any encoding errors
// as fatal errors (we assume that checkConfigValidity has already made
// sure the strings in the config file are reasonable.)
func decodeHexString(s string) []byte {
	r, err := hex.DecodeString(s)
	if err != nil {
		log.Fatalf("unable to decode hex string: %v", err)
	}
	return r
}

// Returns a string that gives the given number of bytes with reasonable
// units. If 'fixedWidth' is true, the returned string will always be the same
// length, which makes it easier to line things up in columns.
func fmtbytes(n int64, fixedWidth bool) string {
	if fixedWidth {
		if n >= 1024*1024*1024*1024 {
			return fmt.Sprintf("%6.2f TiB", float64(n)/(1024.*1024.*
				1024.*1024.))
		} else if n >= 1024*1024*1024 {
			return fmt.Sprintf("%6.2f GiB", float64(n)/(1024.*1024.*
				1024.))
		} else if n > 1024*1024 {
			return fmt.Sprintf("%6.2f MiB", float64(n)/(1024.*1024.))
		} else if n > 1024 {
			return fmt.Sprintf("%6.2f kiB", float64(n)/1024.)
		} else {
			return fmt.Sprintf("%6d B  ", n)
		}
	} else {
		if n >= 1024*1024*1024*1024 {
			return fmt.Sprintf("%.2f TiB", float64(n)/(1024.*1024.*
				1024.*1024.))
		} else if n >= 1024*1024*1024 {
			return fmt.Sprintf("%.2f GiB", float64(n)/(1024.*1024.*
				1024.))
		} else if n > 1024*1024 {
			return fmt.Sprintf("%.2f MiB", float64(n)/(1024.*1024.))
		} else if n > 1024 {
			return fmt.Sprintf("%.2f kiB", float64(n)/1024.)
		} else {
			return fmt.Sprintf("%d B", n)
		}
	}
}

func fmtDuration(d time.Duration) string {
	seconds := int(d.Seconds())
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	var str string
	if hours > 0 {
		str += fmt.Sprintf("%dh ", hours)
	}
	if minutes > 0 {
		str += fmt.Sprintf("%dm ", minutes)
	}
	return str + fmt.Sprintf("%ds", seconds%60)
}

// A few values that printFinalStats() uses to do its work
var startTime = time.Now()
var syncStartTime time.Time
var statsMutex sync.Mutex
var lastStatsTime = time.Now()
var lastStatsBytes int64
var maxActiveBytes int64

func updateActiveMemory() {
	statsMutex.Lock()
	defer statsMutex.Unlock()

	var memstats runtime.MemStats
	runtime.ReadMemStats(&memstats)
	activeBytes := int64(memstats.Alloc)
	if activeBytes > maxActiveBytes {
		maxActiveBytes = activeBytes
	}
}

// Called to print overall statistics after an upload or download is finished.
func printFinalStats() {
	updateActiveMemory()

	statsMutex.Lock()
	defer statsMutex.Unlock()

	syncTime := time.Now().Sub(syncStartTime)
	fmt.Printf("skicka: preparation time %s, sync time %s\n",
		fmtDuration(syncStartTime.Sub(startTime)), fmtDuration(syncTime))
	fmt.Printf("skicka: updated %d Drive files, %d local files\n",
		stats.DriveFilesUpdated, stats.LocalFilesUpdated)
	fmt.Printf("skicka: %s read from disk, %s written to disk\n",
		fmtbytes(stats.DiskReadBytes, false),
		fmtbytes(stats.DiskWriteBytes, false))
	fmt.Printf("skicka: %s uploaded (%s/s), %s downloaded (%s/s)\n",
		fmtbytes(stats.UploadBytes, false),
		fmtbytes(int64(float64(stats.UploadBytes)/syncTime.Seconds()),
			false),
		fmtbytes(stats.DownloadBytes, false),
		fmtbytes(int64(float64(stats.DownloadBytes)/syncTime.Seconds()),
			false))
	fmt.Printf("skicka: %s peak memory used\n",
		fmtbytes(maxActiveBytes, false))
}

func exponentialBackoff(ntries int, resp *http.Response, err error) {
	s := time.Duration(1<<uint(ntries))*time.Second +
		time.Duration(mathrand.Int()%1000)*time.Millisecond
	time.Sleep(s)
	if resp != nil {
		debug.Printf("exponential backoff: slept for resp %d...", resp.StatusCode)
	} else {
		debug.Printf("exponential backoff: slept for error %v...", err)
	}
}

type HTTPResponseResult int

const (
	Success    HTTPResponseResult = iota
	Retry                         = iota
	Fail                          = iota
	RefreshURI                    = iota
)

const maxHTTPRetries = 6

// We've gotten an *http.Response (maybe) and an error (maybe) back after
// performing some HTTP operation; this function takes care of figuring
// out if the operation succeeded, refreshes OAuth2 tokens if expiration
// was the cause of the failure, takes care of exponential back-off for
// transient errors, etc.  It then returns a HTTPResponseResult to the
// caller, indicating how it should proceed.
func handleHTTPResponse(resp *http.Response, err error, ntries int) HTTPResponseResult {
	if err == nil && resp != nil && resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return Success
	}

	if ntries == maxHTTPRetries {
		return Fail
	}

	if resp != nil && resp.StatusCode == 401 {
		// After an hour, the OAuth2 token expires and needs to
		// be refreshed.
		debug.Printf("Trying OAuth2 token refresh.")
		if err = oAuthTransport.Refresh(); err == nil {
			// Success
			return Retry
		}
		// Otherwise fall through to sleep
	}

	// 403, 500, and 503 error codes come up for transient issues like
	// hitting the rate limit for Drive SDK API calls, but sometimes we get
	// other timeouts/connection resets here. Therefore, for all errors, we
	// sleep (with exponential backoff) and try again a few times before
	// giving up.
	exponentialBackoff(ntries, resp, err)
	return Retry
}

///////////////////////////////////////////////////////////////////////////
// Encryption/decryption

// Encrypt the given plaintext using the given encryption key 'key' and
// initialization vector 'iv'. The initialization vector should be 16 bytes
// (the AES block-size), and should be randomly generated and unique for
// each file that's encrypted.
func encryptBytes(key []byte, iv []byte, plaintext []byte) []byte {
	r, _ := ioutil.ReadAll(makeEncrypterReader(key, iv, bytes.NewReader(plaintext)))
	return r
}

// Returns an io.Reader that encrypts the byte stream from the given io.Reader
// using the given key and initialization vector.
func makeEncrypterReader(key []byte, iv []byte, reader io.Reader) io.Reader {
	if key == nil {
		log.Fatalf("uninitialized key in makeEncrypterReader()")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		log.Fatalf("unable to create AES cypher: %v", err)
	}
	if len(iv) != aes.BlockSize {
		log.Fatalf("IV length %d != aes.BlockSize %d", len(iv),
			aes.BlockSize)
	}

	stream := cipher.NewCFBEncrypter(block, iv)
	return &cipher.StreamReader{S: stream, R: reader}
}

// Decrypt the given cyphertext using the given encryption key and
// initialization vector 'iv'.
func decryptBytes(key []byte, iv []byte, ciphertext []byte) []byte {
	r, _ := ioutil.ReadAll(makeDecryptionReader(key, iv, bytes.NewReader(ciphertext)))
	return r
}

func makeDecryptionReader(key []byte, iv []byte, reader io.Reader) io.Reader {
	if key == nil {
		log.Fatalf("uninitialized key in makeDecryptionReader()")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		log.Fatalf("unable to create AES cypher: %v", err)
	}
	if len(iv) != aes.BlockSize {
		log.Fatalf("IV length %d != aes.BlockSize %d", len(iv),
			aes.BlockSize)
	}

	stream := cipher.NewCFBDecrypter(block, iv)
	return &cipher.StreamReader{S: stream, R: reader}
}

// Return the given number of bytes of random values, using a
// cryptographically-strong random number source.
func getRandomBytes(n int) []byte {
	bytes := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, bytes); err != nil {
		log.Fatalf("unable to get random bytes: %v", err)
	}
	return bytes
}

// Create a new encryption key and encrypt it using the user-provided
// passphrase. Prints output to stdout that gives text to add to the
// ~/.skicka.config file to store the encryption key.
func generateKey() {
	passphrase := os.Getenv(passphraseEnvironmentVariable)
	if passphrase == "" {
		printErrorAndExit(fmt.Errorf("skicka: SKICKA_PASSPHRASE " +
			"environment variable not set."))
	}

	// Derive a 64-byte hash from the passphrase using PBKDF2 with 65536
	// rounds of SHA256.
	salt := getRandomBytes(32)
	hash := pbkdf2.Key([]byte(passphrase), salt, 65536, 64, sha256.New)
	if len(hash) != 64 {
		log.Fatalf("incorrect key size returned by pbkdf2 %d", len(hash))
	}

	// We'll store the first 32 bytes of the hash to use to confirm the
	// correct passphrase is given on subsequent runs.
	passHash := hash[:32]
	// And we'll use the remaining 32 bytes as a key to encrypt the actual
	// encryption key. (These bytes are *not* stored).
	keyEncryptKey := hash[32:]

	// Generate a random encryption key and encrypt it using the key
	// derived from the passphrase.
	key := getRandomBytes(32)
	iv := getRandomBytes(16)
	encryptedKey := encryptBytes(keyEncryptKey, iv, key)

	fmt.Printf("; Add the following lines to the [encryption] section\n")
	fmt.Printf("; of your ~/.skicka.config file.\n")
	fmt.Printf("\tsalt=%s\n", hex.EncodeToString(salt))
	fmt.Printf("\tpassphrase-hash=%s\n", hex.EncodeToString(passHash))
	fmt.Printf("\tencrypted-key=%s\n", hex.EncodeToString(encryptedKey))
	fmt.Printf("\tencrypted-key-iv=%s\n", hex.EncodeToString(iv))
}

// Decrypts the encrypted encryption key using values from the config file
// and the user's passphrase.
func decryptEncryptionKey() ([]byte, error) {
	if key != nil {
		log.Fatalf("key aready decrypted!")
	}

	salt := decodeHexString(config.Encryption.Salt)
	passphraseHash := decodeHexString(config.Encryption.Passphrase_hash)
	encryptedKey := decodeHexString(config.Encryption.Encrypted_key)
	encryptedKeyIv := decodeHexString(config.Encryption.Encrypted_key_iv)

	passphrase := os.Getenv(passphraseEnvironmentVariable)
	if passphrase == "" {
		return nil, fmt.Errorf("SKICKA_PASSPHRASE environment " +
			"variable not set")
	}

	derivedKey := pbkdf2.Key([]byte(passphrase), salt, 65536, 64, sha256.New)
	// Make sure the first 32 bytes of the derived key match the bytes stored
	// when we first generated the key; if they don't, the user gave us
	// the wrong passphrase.
	if !bytes.Equal(derivedKey[:32], passphraseHash) {
		return nil, fmt.Errorf("incorrect passphrase")
	}

	// Use the last 32 bytes of the derived key to decrypt the actual
	// encryption key.
	keyEncryptKey := derivedKey[32:]
	decryptedKey := decryptBytes(keyEncryptKey, encryptedKeyIv, encryptedKey)

	return decryptedKey, nil
}

///////////////////////////////////////////////////////////////////////////
// Google Drive utility functions

// Google Drive identifies each file with a unique Id string; this function
// returns the *drive.File corresponding to a given Id, dealing with
// timeouts and transient errors.
func getFileById(id string) (*drive.File, error) {
	for ntries := 0; ; ntries++ {
		file, err := drivesvc.Files.Get(id).Do()
		if err == nil {
			return file, nil
		} else if err = tryToHandleDriveAPIError(err, ntries); err != nil {
			return nil, err
		}
	}
}

func addProperty(prop *drive.Property, driveFile *drive.File) error {
	for ntries := 0; ; ntries++ {
		_, err := drivesvc.Properties.Insert(driveFile.Id, prop).Do()
		if err == nil {
			return nil
		} else if err = tryToHandleDriveAPIError(err, ntries); err != nil {
			return fmt.Errorf("unable to create %s property: %v",
				prop.Key, err)
		}
	}
}

// Execute the given query with the Google Drive API, returning an array of
// files that match the query's conditions. Handles transient HTTP errors and
// the like.
func runDriveQuery(query string) []*drive.File {
	pageToken := ""
	var result []*drive.File
	for {
		q := drivesvc.Files.List().Q(query)
		if pageToken != "" {
			q = q.PageToken(pageToken)
		}

		for ntries := 0; ; ntries++ {
			r, err := q.Do()
			if err == nil {
				result = append(result, r.Items...)
				pageToken = r.NextPageToken
				break
			} else if err = tryToHandleDriveAPIError(err, ntries); err != nil {
				log.Fatalf("couldn't run Google Drive query: %v",
					err)
			}
		}

		if pageToken == "" {
			break
		}
	}
	return result
}

// http://stackoverflow.com/questions/18578768/403-rate-limit-on-insert-sometimes-succeeds
// Sometimes when we get a 403 error from Files.Insert().Do(), a file is
// actually created. Delete the file to be sure we don't have duplicate
// files with the same name.
func deleteIncompleteDriveFiles(title string, parentId string) {
	query := fmt.Sprintf("'%s' in parents and title='%s'", parentId, title)
	files := runDriveQuery(query)
	for _, f := range files {
		for ntries := 0; ; ntries++ {
			err := drivesvc.Files.Delete(f.Id).Do()
			if err == nil {
				return
			} else if err = tryToHandleDriveAPIError(err, ntries); err != nil {
				log.Fatalf("error deleting 403 Google Drive file "+
					"for %s (ID %s): %v", title, f.Id, err)
			}
		}
	}
}

// If we didn't shut down cleanly before, there may be files that
// don't have the various properties we expect. Check for that now
// and patch things up as needed.
func createMissingProperties(f *drive.File, mode os.FileMode, encrypt bool) error {
	if !isFolder(f) {
		if encrypt {
			if _, err := getProperty(f, "IV"); err != nil {
				// Compute a unique IV for the file.
				iv := getRandomBytes(aes.BlockSize)
				ivhex := hex.EncodeToString(iv)

				ivprop := new(drive.Property)
				ivprop.Key = "IV"
				ivprop.Value = ivhex
				debug.Printf("Creating IV property for file %s, "+
					"which doesn't have one.", f.Title)
				err := addProperty(ivprop, f)
				if err != nil {
					return err
				}
			}
		}
	}
	if _, err := getProperty(f, "Permissions"); err != nil {
		syncprop := new(drive.Property)
		syncprop.Key = "Permissions"
		syncprop.Value = fmt.Sprintf("%#o", mode&os.ModePerm)
		debug.Printf("Creating Permissions property for file %s, "+
			"which doesn't have one.", f.Title)
		err := addProperty(syncprop, f)
		if err != nil {
			return err
		}
	}
	return nil
}

// Given an initialized *drive.File structure, create an actual file in Google
// Drive. The returned a *drive.File represents the file in Drive.
func insertNewDriveFile(f *drive.File) (*drive.File, error) {
	for ntries := 0; ; ntries++ {
		r, err := drivesvc.Files.Insert(f).Do()
		if err == nil {
			debug.Printf("Created new Google Drive file for %s: ID=%s",
				f.Title, r.Id)
			return r, nil
		}
		debug.Printf("Error %v trying to create drive file for %s. "+
			"Deleting detrius...", err, f.Title)
		deleteIncompleteDriveFiles(f.Title, f.Parents[0].Id)
		err = tryToHandleDriveAPIError(err, ntries)
		if err != nil {
			return nil, fmt.Errorf("unable to create drive.File: %v", err)
		}
	}
}

// Create a new *drive.File with the given name inside the folder represented
// by parentFolder.
func createDriveFile(filename string, mode os.FileMode, modTime time.Time, encrypt bool,
	parentFolder *drive.File) (*drive.File, error) {
	var proplist []*drive.Property
	if encrypt {
		// Compute a unique IV for the file.
		iv := getRandomBytes(aes.BlockSize)
		ivhex := hex.EncodeToString(iv)

		ivprop := new(drive.Property)
		ivprop.Key = "IV"
		ivprop.Value = ivhex
		proplist = append(proplist, ivprop)
	}
	permprop := new(drive.Property)
	permprop.Key = "Permissions"
	permprop.Value = fmt.Sprintf("%#o", mode&os.ModePerm)
	proplist = append(proplist, permprop)

	folderParent := &drive.ParentReference{Id: parentFolder.Id}
	f := &drive.File{
		Title:        filepath.Base(filename),
		MimeType:     "application/octet-stream",
		Parents:      []*drive.ParentReference{folderParent},
		ModifiedDate: modTime.UTC().Format(timeFormat),
		Properties:   proplist,
	}
	debug.Printf("inserting %#v", f)

	return insertNewDriveFile(f)
}

// Create a *drive.File for the folder with the given title and parent folder.
func createDriveFolder(title string, mode os.FileMode, modTime time.Time,
	parentFolder *drive.File) (*drive.File, error) {
	var proplist []*drive.Property
	permprop := new(drive.Property)
	permprop.Key = "Permissions"
	permprop.Value = fmt.Sprintf("%#o", mode&os.ModePerm)
	proplist = append(proplist, permprop)

	parentref := &drive.ParentReference{Id: parentFolder.Id}
	f := &drive.File{
		Title:        title,
		MimeType:     "application/vnd.google-apps.folder",
		ModifiedDate: modTime.UTC().Format(timeFormat),
		Parents:      []*drive.ParentReference{parentref},
		Properties:   proplist,
	}

	f, err := insertNewDriveFile(f)
	if err != nil {
		return nil, err
	}
	return f, nil
}

type fileNotFoundError struct {
	path        string
	invokingCmd string
}

func (err fileNotFoundError) Error() string {
	msg := ""
	if err.invokingCmd != "" {
		msg += fmt.Sprintf("%s: ", err.invokingCmd)
	}
	return fmt.Sprintf("%s%s: No such file or directory", msg, err.path)
}

type removeDirectoryError struct {
	path        string
	invokingCmd string
}

func (err removeDirectoryError) Error() string {
	msg := ""
	if err.invokingCmd != "" {
		msg += fmt.Sprintf("%s: ", err.invokingCmd)
	}
	return fmt.Sprintf("%s%s: is a directory", msg, err.path)
}

// Returns the *drive.File corresponding to a given path starting from the
// root of the Google Drive filesystem.  (Note that *drive.File is used to
// represent both files and folders in Google Drive.)
func getDriveFile(path string) (*drive.File, error) {
	parent, err := getFileById("root")
	if err != nil {
		return nil, fmt.Errorf("unable to get Drive root directory: %v", err)
	}

	dirs := strings.Split(path, "/")
	// Walk through the directories in the path in turn.
	for _, dir := range dirs {
		if dir == "" {
			// The first string in the split is "" if the
			// path starts with a '/'.
			continue
		}

		query := fmt.Sprintf("title='%s' and '%s' in parents and trashed=false",
			dir, parent.Id)
		files := runDriveQuery(query)

		if len(files) == 0 {
			return nil, fileNotFoundError{
				path: path,
			}
		} else if len(files) > 1 {
			return nil, fmt.Errorf("%s: multiple files found", path)
		} else {
			parent = files[0]
		}
	}
	return parent, nil
}

// Add all of the the *drive.Files in 'parentFolder' to the provided map from
// pathnames to *driveFiles. Assumes that 'path' is the path down to
// 'parentFolder' when constructing pathnames of files. If 'recursive' is true,
// also includes all files in the full hierarchy under the given folder.
// Otherwise, only the files directly in the folder are returned.
func getFolderContents(path string, parentFolder *drive.File, recursive bool,
	existingFiles map[string]*drive.File) error {
	query := fmt.Sprintf("trashed=false and '%s' in parents", parentFolder.Id)
	dirfiles := runDriveQuery(query)
	for _, f := range dirfiles {
		filepath := filepath.Clean(path + "/" + f.Title)
		if _, ok := existingFiles[filepath]; ok == true {
			// This shouldn't happen in principle, but Drive does
			// allow multiple files to have the same title. It's not
			// obvious how to reconcile this with Unix file
			// semantics, so we just disallow it entirely.
			return fmt.Errorf("%s: duplicate file found on Google Drive",
				filepath)
		}
		existingFiles[filepath] = f
		if isFolder(f) && recursive {
			err := getFolderContents(filepath, f, recursive, existingFiles)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Returns a map from strings to *drive.Files that represents all existing
// files in Google Drive at the folder identified by 'path'. If 'recursive' is
// true, directories under the given path are processed recursively.
// 'includeBase' indicates whether the *drive.File for the given path should
// be included in the result, and 'mustExist' indicates whether an error
// should be returned if the given path doesn't exist on Drive.
func getFilesAtRemotePath(path string, recursive, includeBase,
	mustExist bool) map[string]*drive.File {
	existingFiles := make(map[string]*drive.File)
	file, err := getDriveFile(path)
	if err != nil {
		if !mustExist {
			return existingFiles
		}
		printErrorAndExit(fmt.Errorf("skicka: %v", err))
	}

	if isFolder(file) {
		err := getFolderContents(path, file, recursive, existingFiles)
		if err != nil {
			printErrorAndExit(fmt.Errorf("skicka: %v", err))
		}
		if includeBase {
			existingFiles[path] = file
		}
	} else {
		existingFiles[path] = file
	}
	timeDelta("Get file descriptors from Google Drive")
	return existingFiles
}

// Google Drive files can have properties associated with them, which are
// basically maps from strings to strings. Given a Google Drive file and a
// property name, this function returns the property value, if the named
// property is present.
func getProperty(driveFile *drive.File, name string) (string, error) {
	for _, prop := range driveFile.Properties {
		if prop.Key == name {
			return prop.Value, nil
		}
	}
	return "", fmt.Errorf("%s: property not found", name)
}

// Returns the initialization vector (for encryption) for the given file.
// We store the initialization vector as a hex-encoded property in the
// file so that we don't need to download the file's contents to find the
// IV.
func getInitializationVector(driveFile *drive.File) ([]byte, error) {
	ivhex, err := getProperty(driveFile, "IV")
	if err != nil {
		return nil, err
	}
	iv, err := hex.DecodeString(ivhex)
	if err != nil {
		return nil, err
	}
	if len(iv) != aes.BlockSize {
		return nil, fmt.Errorf("unexpected length of IV %d", len(iv))
	}
	return iv, nil
}

func updateModificationTime(driveFile *drive.File, t time.Time) error {
	debug.Printf("updating modification time of %s to %v", driveFile.Title, t)

	for ntries := 0; ; ntries++ {
		f := &drive.File{ModifiedDate: t.UTC().Format(timeFormat)}
		_, err := drivesvc.Files.Patch(driveFile.Id, f).SetModifiedDate(true).Do()
		if err == nil {
			debug.Printf("success: updated modification time on %s", driveFile.Title)
			return nil
		} else if err = tryToHandleDriveAPIError(err, ntries); err != nil {
			return err
		}
	}
}

func updatePermissions(driveFile *drive.File, mode os.FileMode) error {
	bits := mode & os.ModePerm
	bitsString := fmt.Sprintf("%#o", bits)
	return updateProperty(driveFile, "Permissions", bitsString)
}

func updateProperty(driveFile *drive.File, name string, newValue string) error {
	if oldValue, err := getProperty(driveFile, name); err == nil {
		if oldValue == newValue {
			return nil
		}
	}

	for nTriesGet := 0; ; nTriesGet++ {
		prop, err := drivesvc.Properties.Get(driveFile.Id, name).Do()
		if err == nil {
			prop.Value = newValue
			for nTriesUpdate := 0; ; nTriesUpdate++ {
				_, err = drivesvc.Properties.Update(driveFile.Id,
					name, prop).Do()
				if err == nil {
					// success
					return nil
				} else if err = tryToHandleDriveAPIError(err,
					nTriesUpdate); err != nil {
					return err
				}
			}
		} else if err = tryToHandleDriveAPIError(err, nTriesGet); err != nil {
			return err
		}
	}
}

func getModificationTime(driveFile *drive.File) (time.Time, error) {
	if driveFile.ModifiedDate != "" {
		return time.Parse(time.RFC3339Nano, driveFile.ModifiedDate)
	}
	return time.Unix(0, 0), nil
}

func getPermissions(driveFile *drive.File) (os.FileMode, error) {
	permStr, err := getProperty(driveFile, "Permissions")
	if err != nil {
		return 0, err
	}
	perm, err := strconv.ParseInt(permStr, 8, 16)
	return os.FileMode(perm), err
}

func getResumableUploadURI(driveFile *drive.File, contentType string,
	length int64) (string, error) {
	params := make(url.Values)
	params.Set("uploadType", "resumable")

	urls := fmt.Sprintf("https://www.googleapis.com/upload/drive/v2/files/%s",
		driveFile.Id)
	urls += "?" + params.Encode()

	body, err := googleapi.WithoutDataWrapper.JSONReader(driveFile)
	if err != nil {
		return "", err
	}

	req, _ := http.NewRequest("PUT", urls, body)
	req.Header.Set("X-Upload-Content-Length", fmt.Sprintf("%d", length))
	req.Header.Set("X-Upload-Content-Type", contentType)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("User-Agent", "skicka/0.1")
	// We actually don't need any content in the request, since we're
	// PUTing to an existing file.

	for ntries := 0; ; ntries++ {
		debug.Printf("Trying to get session URI")
		resp, err := oAuthTransport.RoundTrip(req)

		if err == nil && resp != nil && resp.StatusCode == 200 {
			uri := resp.Header["Location"][0]
			debug.Printf("Got resumable upload URI %s", uri)
			return uri, nil
		}
		if err != nil {
			debug.Printf("getResumableUploadURI: %v", err)
		}
		if resp != nil {
			b, _ := ioutil.ReadAll(resp.Body)
			debug.Printf("getResumableUploadURI status %d\n"+
				"Resp: %+v\nBody: %s", resp.StatusCode, *resp, b)
		}
		if ntries == 5 {
			// Give up...
			return "", err
		}

		exponentialBackoff(ntries, resp, err)
	}
}

// In certain error cases, we need to go back and query Drive as to how
// much of a file has been successfully uploaded (and thence where we
// should start for the next chunk.)  This function generates that query
// and updates the provided *currentOffset parameter with the result.
func getCurrentChunkStart(sessionURI string, contentLength int64,
	currentOffset *int64) (HTTPResponseResult, error) {
	var err error
	for r := 0; r < 6; r++ {
		req, _ := http.NewRequest("PUT", sessionURI, nil)
		req.Header.Set("Content-Range", fmt.Sprintf("bytes */%d", contentLength))
		req.Header.Set("Content-Length", "0")
		req.ContentLength = 0
		req.Header.Set("User-Agent", "skicka/0.1")
		resp, err := oAuthTransport.RoundTrip(req)

		if resp == nil {
			debug.Printf("get current chunk start err %v", err)
			exponentialBackoff(r, resp, err)
			continue
		}

		defer resp.Body.Close()
		b, _ := ioutil.ReadAll(resp.Body)
		debug.Printf("Get current chunk start err %v resp status %d, "+
			"body %s\nRESP %v",
			err, resp.StatusCode, b, *resp)

		if resp.StatusCode == 200 || resp.StatusCode == 201 {
			// 200 or 201 here says we're actually all done
			debug.Printf("All done: %d from get content-range response",
				resp.StatusCode)
			return Success, nil
		} else if resp.StatusCode == 308 {
			*currentOffset, err = updateStartFromResponse(resp)
			if err != nil {
				return Fail, err
			}
			debug.Printf("Updated start to %d after 308 from get "+
				"content-range...", *currentOffset)
			return Retry, nil
		} else if resp.StatusCode == 401 {
			debug.Printf("Trying OAuth2 token refresh.")
			for r := 0; r < 6; r++ {
				if err = oAuthTransport.Refresh(); err == nil {
					debug.Printf("Token refresh success")
					// Now once again try the PUT...
					break
				} else {
					debug.Printf("refresh try %d fail %v", r, err)
					exponentialBackoff(r, nil, err)
				}
			}
		}
	}
	debug.Printf("couldn't recover from 503...")
	return Fail, err
}

// The response we get back from uploading a file chunk includes a "Range"
// field, which gives the range (inclusive!) of bytes that actually were
// successfully uploaded; the ending byte offset may be before the end of
// the range we tried to upload, if there was an error partway through.
// This function returns this offset, so that the next chunk upload can
// start at the right place.
func updateStartFromResponse(resp *http.Response) (int64, error) {
	if rangeString, ok := resp.Header["Range"]; ok && len(rangeString) > 0 {
		var rangeStart, rangeEnd int64
		fmt.Sscanf(rangeString[0], "bytes=%d-%d", &rangeStart, &rangeEnd)
		return rangeEnd + 1, nil
	}
	return 0, fmt.Errorf("Malformed HTTP response to get range %v", *resp)
}

// When we upload a file chunk, a variety of responses may come back from
// the server, ranging from permanent errors to transient errors, to
// success codes.  This function processes the http.Response and maps it to
// a HTTPResponseResult code.  It also may update *ntries, the conut of how
// many times we've tried in a row to upload a chunk, *start, the current
// offset into the file being uploaded, and *sessionURI, the URI to which
// chunks for the file should be uploaded to.
func handleResumableUploadResponse(resp *http.Response, err error, driveFile *drive.File,
	contentType string, contentLength int64, ntries *int, currentOffset *int64,
	sessionURI *string) (HTTPResponseResult, error) {
	if *ntries == 6 {
		if err != nil {
			return Fail, fmt.Errorf("giving up after 6 retries: %v", err)
		} else if resp.StatusCode == 403 {
			return Fail, fmt.Errorf("giving up after 6 retries: " +
				"rate limit exceeded")
		} else {
			return Fail, fmt.Errorf("giving up after 6 retries: %s",
				resp.Status)
		}
	}

	// Serious error (e.g. connection reset) where we didn't even get a
	// HTTP response back from the server.  Try again (a few times).
	if err != nil {
		debug.Printf("handleResumableUploadResponse error %v", err)
		exponentialBackoff(*ntries, resp, err)
		return Retry, nil
	}

	debug.Printf("got status %d from chunk for file %s", resp.StatusCode,
		driveFile.Id, resp)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		// Successfully uploaded the entire file.
		return Success, nil

	case resp.StatusCode == 308:
		// This is the expected response when a chunk was uploaded
		// successfully, but there are still more chunks to do
		// before we're done.
		*currentOffset, err = updateStartFromResponse(resp)
		if err != nil {
			return Fail, err
		}
		*ntries = 0
		debug.Printf("Updated currentOffset to %d after 308", *currentOffset)
		return Retry, nil

	case resp.StatusCode == 404:
		// The upload URI has expired; we need to refresh it. (It
		// has a ~24 hour lifetime.)
		*sessionURI, err = getResumableUploadURI(driveFile,
			contentType, contentLength)
		debug.Printf("Got %v after updating URI from 404...", err)
		if err != nil {
			return Fail, err
		}

		// Use the new URI to find the offset to start at.
		*ntries = 0
		return getCurrentChunkStart(*sessionURI, contentLength,
			currentOffset)

	case resp.StatusCode == 401:
		// After an hour, the OAuth2 token expires and needs to
		// be refreshed.
		debug.Printf("Trying OAuth2 token refresh.")
		for r := 0; r < 6; r++ {
			if err = oAuthTransport.Refresh(); err == nil {
				// Successful refresh; make sure we have
				// the right offset for the next time
				// around.
				return getCurrentChunkStart(*sessionURI, contentLength,
					currentOffset)
			}
			debug.Printf("Token refresh fail %v", err)
			exponentialBackoff(r, nil, err)
		}
		return Fail, err

	case resp.StatusCode >= 500 && resp.StatusCode <= 599:
		debug.Printf("5xx response")
		return getCurrentChunkStart(*sessionURI, contentLength, currentOffset)

	default:
		exponentialBackoff(*ntries, resp, err)
		return Retry, nil
	}
}

func uploadFileContentsResumable(driveFile *drive.File, contentsReader io.Reader,
	contentLength int64) error {
	contentsReader, contentType, err := detectContentType(contentsReader)
	if err != nil {
		return err
	}

	sessionURI, err := getResumableUploadURI(driveFile, contentType,
		contentLength)
	if err != nil {
		return err
	}

	// TODO: what is a reasonable default here? Must be 256kB minimum.
	chunkSize := 1024 * 1024

	seekableReader := MakeSomewhatSeekableReader(contentsReader, 2*chunkSize)

	// Upload the file in chunks of size chunkSize (or smaller, for the
	// very last chunk).
	for currentOffset, ntries := int64(0), 0; currentOffset < contentLength; ntries++ {
		end := currentOffset + int64(chunkSize)
		if end > contentLength {
			end = contentLength
		}
		debug.Printf("%s: uploading chunk %d - %d...", driveFile.Title,
			currentOffset, end)

		// We should usually already be at the current offset; this
		// seek should be a no-op except in cases where the
		// previous chunk had an error.
		err = seekableReader.SeekTo(currentOffset)
		if err != nil {
			return err
		}

		// Only allow the current range of bytes to be uploaded
		// with this PUT.
		var body io.Reader = &io.LimitedReader{
			R: seekableReader,
			N: end - currentOffset,
		}
		if config.Upload.Bytes_per_second_limit > 0 {
			body = &RateLimitedReader{R: body}
		}

		all, err := ioutil.ReadAll(body)
		if int64(len(all)) != end-currentOffset {
			log.Fatalf("reader gave us %d bytes, expected %d, bye", len(all),
				end-currentOffset)
		}
		req, _ := http.NewRequest("PUT", sessionURI, bytes.NewReader(all))

		//		req, _ := http.NewRequest("PUT", sessionURI, body)
		req.ContentLength = int64(end - currentOffset)
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", currentOffset, end-1, contentLength))
		req.Header.Set("User-Agent", "skicka/0.1")

		// Actually (try to) upload the chunk.
		resp, err := oAuthTransport.RoundTrip(req)

		origCurrentOffset := currentOffset
		status, err := handleResumableUploadResponse(resp, err,
			driveFile, contentType, contentLength, &ntries, &currentOffset,
			&sessionURI)

		if resp != nil {
			googleapi.CloseBody(resp)
		}
		if status == Fail {
			return err
		}

		if status == Success {
			// The entire file has been uploaded successfully.
			atomic.AddInt64(&stats.UploadBytes, end-currentOffset)
			atomic.AddInt64(&stats.DriveFilesUpdated, 1)
			return nil
		}

		// We only account for the number of actually uploaded
		// bytes by using the updated version of currentOffset
		// here. For cases where only a partial chunk is uploaded,
		// the progress bar may reflect more data being uploaded
		// than actually was, due to it being updated based on what
		// has been consumed from the original Reader. This is
		// probably fine; things get back in sync once we work
		// through the bits buffered in the SomewhatSeekableReader.
		atomic.AddInt64(&stats.UploadBytes, currentOffset-origCurrentOffset)

		// Go around again and do the next chunk...
	}

	// This should perhaps be a panic, as if we are able to upload all
	// of the data but then the Drive API doesn't give us a 2xx reply
	// with the last chunk, then something is really broken.
	return fmt.Errorf("uploaded entire file but didn't get 2xx status on last chunk")
}

func detectContentType(contentsReader io.Reader) (io.Reader, string, error) {
	// Grab the start of the contents so that we can try to identify
	// the content type.
	contentsHeader := make([]byte, 512)
	headerLength, err := contentsReader.Read(contentsHeader)
	if err != nil {
		if err.Error() == "EOF" {
			// Empty file; this is fine, and we're done.
			return nil, "", nil
		}
		return nil, "", err
	}
	contentType := http.DetectContentType(contentsHeader)

	// Reconstruct a new Reader that returns the same byte stream
	// as the original one, effectively pasting the bytes we read for
	// the content-type identification to the start of what remains in
	// the original io.Reader.
	contentsReader = io.MultiReader(bytes.NewReader(contentsHeader[:headerLength]),
		contentsReader)

	return contentsReader, contentType, nil
}

func prepareUploadPUT(driveFile *drive.File, contentsReader io.Reader,
	length int64) (*http.Request, error) {
	params := make(url.Values)
	params.Set("uploadType", "media")

	urls := fmt.Sprintf("https://www.googleapis.com/upload/drive/v2/files/%s",
		url.QueryEscape(driveFile.Id))
	urls += "?" + params.Encode()

	contentsReader, contentType, err := detectContentType(contentsReader)
	if err != nil {
		return nil, err
	}

	req, _ := http.NewRequest("PUT", urls, contentsReader)
	googleapi.SetOpaque(req.URL)
	req.ContentLength = length
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "skicka/0.1")

	return req, nil
}

// Upload the file contents given by the io.Reader to the given *drive.File.
func uploadFileContents(driveFile *drive.File, contentsReader io.Reader,
	length int64, currentTry int) error {
	// Only run the resumable upload path for large files (it
	// introduces some overhead that isn't worth it for smaller files.)
	if length > resumableUploadMinSize {
		return uploadFileContentsResumable(driveFile, contentsReader, length)
	}

	// Limit upload bandwidth, if requested..
	if config.Upload.Bytes_per_second_limit > 0 {
		contentsReader = &RateLimitedReader{R: contentsReader}
	}

	// Get the PUT request for the upload.
	req, err := prepareUploadPUT(driveFile, contentsReader, length)
	if err != nil {
		return err
	}
	if req == nil {
		// Empty file--we're done.
		atomic.AddInt64(&stats.DriveFilesUpdated, 1)
		return nil
	}

	// And send it off...
	resp, err := oAuthTransport.RoundTrip(req)
	if resp != nil {
		defer googleapi.CloseBody(resp)
	}

	switch handleHTTPResponse(resp, err, currentTry) {
	case Success:
		debug.Printf("Success for %s: code %d", driveFile.Title, resp.StatusCode)
		atomic.AddInt64(&stats.UploadBytes, length)
		atomic.AddInt64(&stats.DriveFilesUpdated, 1)
		return nil
	case Fail:
		if err == nil {
			log.Fatalf("nil err but fail? resp %v", *resp)
		}
		return err
	case Retry:
		// Otherwise tell the caller to please set up the reader, etc.,
		// again and retry...
		if resp != nil {
			b, _ := ioutil.ReadAll(resp.Body)
			return RetryHTTPTransmitError{StatusCode: resp.StatusCode,
				StatusBody: string(b)}
		}
		return RetryHTTPTransmitError{StatusCode: 500, StatusBody: err.Error()}
	default:
		panic("Unhandled HTTPResult value in switch")
	}
}

// Get the contents of the *drive.File as an io.ReadCloser.
func getDriveFileContentsReader(driveFile *drive.File) (io.ReadCloser, error) {
	// The file download URL expires some hours after it's retrieved;
	// re-grab the file right before downloading it so that we have a
	// fresh URL.
	driveFile, err := getFileById(driveFile.Id)
	if err != nil {
		return nil, err
	}

	url := driveFile.DownloadUrl

	if url == "" {
		url = driveFile.ExportLinks[driveFile.MimeType]
	}

	for ntries := 0; ; ntries++ {
		request, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, err
		}

		resp, err := oAuthTransport.RoundTrip(request)

		switch handleHTTPResponse(resp, err, ntries) {
		case Success:
			return resp.Body, nil
		case Fail:
			return nil, err
		case Retry:
		}
	}
}

// There are a number of cases where the Google Drive API returns an error
// code but where it's possible to recover from the error; examples include
// 401 errors when the OAuth2 token expires after an hour, or 403/500 errors
// when we make too many API calls too quickly and we get a rate limit error.
// This function takes an error returned by a Drive API call and the number
// of times that we've tried to call the API entrypoint already and does
// its best to handle the error.
//
// If it thinks it may have been successful, it returns nil, and the caller
// should try the call again. For unrecoverable errors (or too many errors
// in a row), it returns the error code back and the caller should stop trying.
func tryToHandleDriveAPIError(err error, ntries int) error {
	debug.Printf("tryToHandleDriveAPIError: ntries %d error %T %+v",
		ntries, err, err)

	maxAPIRetries := 6
	if ntries == maxAPIRetries {
		return err
	}
	switch err := err.(type) {
	case *googleapi.Error:
		if err.Code == 401 {
			// After an hour, the OAuth2 token expires and needs to
			// be refreshed.
			debug.Printf("Trying OAuth2 token refresh.")
			if err := oAuthTransport.Refresh(); err == nil {
				// Success
				return nil
			}
			// Otherwise fall through to sleep/backoff...
		}
	}

	exponentialBackoff(ntries, nil, err)
	return nil
}

func createDriveClient(clientid, clientsecret, cacheFile string) error {
	config := &oauth.Config{
		ClientId:     clientid,
		ClientSecret: clientsecret,
		Scope:        "https://www.googleapis.com/auth/drive",
		RedirectURL:  "urn:ietf:wg:oauth:2.0:oob",
		AuthURL:      "https://accounts.google.com/o/oauth2/auth",
		TokenURL:     "https://accounts.google.com/o/oauth2/token",
		TokenCache:   oauth.CacheFile(cacheFile),
	}

	oAuthTransport = &oauth.Transport{
		Config:    config,
		Transport: http.DefaultTransport,
	}

	token, err := config.TokenCache.Token()
	if err != nil {
		authURL := config.AuthCodeURL("state")
		fmt.Printf("Go to the following link in your browser:\n%v\n", authURL)
		fmt.Printf("Enter verification code: ")
		var code string
		fmt.Scanln(&code)
		token, err = oAuthTransport.Exchange(code)
		if err != nil {
			log.Fatalf("OAuth2 exchange failed: %v\n", err)
		}
	}
	oAuthTransport.Token = token

	drivesvc, err = drive.New(oAuthTransport.Client())
	return err
}

func isFolder(f *drive.File) bool {
	return f.MimeType == "application/vnd.google-apps.folder"
}

///////////////////////////////////////////////////////////////////////////
// Uploading files and directory hierarchies to Google Drive

// Representation of a local file that may need to be synced up to Drive.
type LocalToRemoteFileMapping struct {
	LocalPath     string
	RemotePath    string
	LocalFileInfo os.FileInfo
}

// Implement sort.Interface so that we can sort arrays of
// LocalToRemoteFileMapping by file size.
type LocalToRemoteBySize []LocalToRemoteFileMapping

func (l2r LocalToRemoteBySize) Len() int      { return len(l2r) }
func (l2r LocalToRemoteBySize) Swap(i, j int) { l2r[i], l2r[j] = l2r[j], l2r[i] }
func (l2r LocalToRemoteBySize) Less(i, j int) bool {
	return l2r[i].LocalFileInfo.Size() < l2r[j].LocalFileInfo.Size()
}

// Perform fairly inexpensive comparisons of the file size and last modification
// time metadata of the local file and the corresponding file on Google Drive.
// If these values match, we assume that the two files are consistent and don't
// examine the local file contents further to see if an upload is required
// (unless the -ignore-times flag has been used).
func fileMetadataMatches(info os.FileInfo, encrypt bool,
	driveFile *drive.File) (bool, error) {
	localSize := info.Size()
	driveSize := driveFile.FileSize
	if encrypt {
		// We store a copy of the initialization vector at the start of
		// the file stored in Google Drive; account for this when
		// comparing the file sizes.
		driveSize -= aes.BlockSize
	}
	if localSize != driveSize {
		// File sizes mismatch; update needed.
		return false, nil
	}

	driveTime, err := getModificationTime(driveFile)
	if err != nil {
		return true, err
	}

	// Finally, check if the local modification time is different than the
	// modification time of the file the last time it was updated on Drive;
	// if it is, we return false and an upload will be done..
	localTime := info.ModTime()
	debug.Printf("localTime: %v, driveTime: %v", localTime, driveTime)
	return localTime.Equal(driveTime), nil
}

// Return the md5 hash of the file at the given path in the form of a
// string. If encryption is enabled, use the encrypted file contents when
// computing the hash.
func localFileMD5Contents(path string, encrypt bool, iv []byte) (string, error) {
	contentsReader, _, err := getFileContentsReaderForUpload(path, encrypt, iv)
	if contentsReader != nil {
		defer contentsReader.Close()
	}
	if err != nil {
		return "", err
	}

	md5 := md5.New()
	n, err := io.Copy(md5, contentsReader)
	if err != nil {
		return "", err
	}
	atomic.AddInt64(&stats.DiskReadBytes, n)

	return fmt.Sprintf("%x", md5.Sum(nil)), nil
}

// Returns an io.ReadCloser for given file, such that the bytes read are
// ready for upload: specifically, if encryption is enabled, the contents
// are encrypted with the given key and the initialization vector is
// prepended to the returned bytes. Otherwise, the contents of the file are
// returned directly.
func getFileContentsReaderForUpload(path string, encrypt bool,
	iv []byte) (io.ReadCloser, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return f, 0, err
	}

	stat, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	fileSize := stat.Size()

	if encrypt {
		if key == nil {
			key, err = decryptEncryptionKey()
			if err != nil {
				return nil, 0, err
			}
		}

		r := makeEncrypterReader(key, iv, f)

		// Prepend the initialization vector to the returned bytes.
		r = io.MultiReader(bytes.NewReader(iv[:aes.BlockSize]), r)

		return &FileCloser{R: r, C: f}, fileSize + aes.BlockSize, nil
	}
	return f, fileSize, nil
}

// Given a file on the local disk, synchronize it with Google Drive: if the
// corresponding file doesn't exist on Drive, it's created; if it exists
// but has different contents, the contents are updated.  The Unix
// permissions and file modification time on Drive are also updated
// appropriately.
// Besides being sent up to Google Drive, the file is tee'd (via io.Tee)
// into an optional writer variable.  This variable can safely be nil.
func syncFileUp(fileMapping LocalToRemoteFileMapping, encrypt bool,
	existingDriveFiles map[string]*drive.File, pb *pb.ProgressBar) error {
	debug.Printf("syncFileUp: %#v", fileMapping.LocalFileInfo)

	// We need to create the file or folder on Google Drive.
	var err error

	// Get the *drive.File for the folder to create the new file in.
	// This folder should definitely exist at this point, since we
	// create all folders needed before starting to upload files.
	dirPath := filepath.Dir(fileMapping.RemotePath)
	if dirPath == "." {
		dirPath = "/"
	}
	parentFile, ok := existingDriveFiles[dirPath]
	if !ok {
		parentFile, err = getDriveFile(dirPath)
		if err != nil {
			// We can't really recover at this point; the
			// parent folder definitely should have been
			// created by now, and we can't proceed without
			// it...
			printErrorAndExit(fmt.Errorf("skicka: %v", err))
		}
	}

	baseName := filepath.Base(fileMapping.RemotePath)
	var driveFile *drive.File

	if fileMapping.LocalFileInfo.IsDir() {
		driveFile, err = createDriveFolder(baseName,
			fileMapping.LocalFileInfo.Mode(), fileMapping.LocalFileInfo.ModTime(),
			parentFile)
		if err != nil {
			return err
		}
		atomic.AddInt64(&stats.UploadBytes, fileMapping.LocalFileInfo.Size())
		pb.Increment()
		verbose.Printf("Created Google Drive folder %s", fileMapping.RemotePath)

		// We actually only update the map when we create new folders;
		// we don't update it for new files.  There are two reasons
		// for this: first, once we've created a file, we don't
		// access it again during a given run of skicka.
		// Second, file upload is multi-threaded, and would require a
		// mutex to the map, which doesn't seem worth the trouble
		// given the first reason.
		//
		// Note that if the contents of Google Drive are modified in
		// another session, this map may become stale; we don't
		// explicitly look out for this and will presumably error out
		// in interesting ways if it does happen.
		existingDriveFiles[fileMapping.RemotePath] = driveFile
	} else {
		if driveFile, ok = existingDriveFiles[fileMapping.RemotePath]; !ok {
			driveFile, err = createDriveFile(baseName,
				fileMapping.LocalFileInfo.Mode(),
				fileMapping.LocalFileInfo.ModTime(), encrypt, parentFile)
			if err != nil {
				return err
			}
		}

		var iv []byte
		if encrypt {
			iv, err = getInitializationVector(driveFile)
			if err != nil {
				return fmt.Errorf("unable to get IV: %v", err)
			}
		}

		for ntries := 0; ntries < 5; ntries++ {
			var reader io.Reader
			var countingReader *ByteCountingReader

			contentsReader, length, err :=
				getFileContentsReaderForUpload(fileMapping.LocalPath, encrypt, iv)
			if contentsReader != nil {
				defer contentsReader.Close()
			}
			if err != nil {
				return err
			}
			reader = contentsReader

			if pb != nil {
				countingReader = &ByteCountingReader{
					R: reader,
				}
				reader = io.TeeReader(countingReader, pb)
			}

			err = uploadFileContents(driveFile, reader, length, ntries)
			if err == nil {
				// Success!
				break
			}

			if re, ok := err.(RetryHTTPTransmitError); ok {
				debug.Printf("%s: got retry http error--retrying: %s",
					fileMapping.LocalPath, re.Error())
				if pb != nil {
					// The "progress" made so far on
					// this file should be rolled back
					pb.Add64(int64(0 - countingReader.bytesRead))
				}
			} else {
				debug.Printf("%s: giving up due to error: %v",
					fileMapping.LocalPath, err)
				// This file won't be uploaded, so subtract
				// the expected progress from the total
				// expected bytes
				if pb != nil {
					pb.Add64(int64(0 - countingReader.bytesRead))
					pb.Total -= length
				}
				return err
			}
		}
	}

	verbose.Printf("Updated local %s -> Google Drive %s", fileMapping.LocalPath,
		fileMapping.RemotePath)
	return updateModificationTime(driveFile, fileMapping.LocalFileInfo.ModTime())
}

func checkFatalError(err error, message string) {
	if err != nil {
		printErrorAndExit(fmt.Errorf(message, err))
	}
}

// Synchronize a local directory hierarchy with Google Drive.
// localPath is the file or directory to start with, driveRoot is
// the directory into which the file/directory will be sent
func syncHierarchyUp(localPath string, driveRoot string,
	existingFiles map[string]*drive.File,
	encrypt bool, ignoreTimes bool) error {
	if encrypt {
		var err error
		key, err = decryptEncryptionKey()
		if err != nil {
			return err
		}
	}

	// Kick off a background thread to periodically allow uploading
	// a bit more data.  This allowance is consumed by the
	// RateLimitedReader Read() function.
	launchBandwidthTask(config.Upload.Bytes_per_second_limit)

	fileMappings, err := compileUploadFileTree(localPath, driveRoot, encrypt)
	checkFatalError(err, "skicka: error getting local filetree: %v")
	timeDelta("Walk local directories")
	fileMappings, err = filterFilesToUpload(fileMappings, existingFiles, encrypt,
		ignoreTimes)
	checkFatalError(err, "skicka: error determining files to sync: %v")

	if len(fileMappings) == 0 {
		fmt.Fprintln(os.Stderr,
			"skicka: there are no new files that need to be uploaded.")
		return nil
	}

	nBytesToUpload := int64(0)
	for _, info := range fileMappings {
		if !info.LocalFileInfo.IsDir() {
			nBytesToUpload += info.LocalFileInfo.Size()
		}
	}

	// Given the list of files to sync, first find all of the directories and
	// then either get or create a Drive folder for each one.
	directoryMappingMap := make(map[string]LocalToRemoteFileMapping)
	var directoryNames []string
	for _, localfile := range fileMappings {
		if localfile.LocalFileInfo.IsDir() {
			directoryNames = append(directoryNames, localfile.RemotePath)
			directoryMappingMap[localfile.RemotePath] = localfile
		}
	}

	// Now sort the directories by name, which ensures that the parent of each
	// directory is available if we need to create its children.
	sort.Strings(directoryNames)

	nUploadErrors := int32(0)

	if len(directoryNames) > 0 {
		dirProgressBar := pb.New(len(directoryNames))
		dirProgressBar.ShowBar = true
		dirProgressBar.Output = os.Stderr
		dirProgressBar.Prefix("Directories: ")
		dirProgressBar.Start()

		// And finally sync the directories, which serves to create any missing ones.
		for _, dirName := range directoryNames {
			file := directoryMappingMap[dirName]
			err = syncFileUp(file, encrypt, existingFiles, dirProgressBar)
			if err != nil {
				nUploadErrors++
				printErrorAndExit(fmt.Errorf("skicka: %s: %v", file.LocalPath, err))
			}
			updateActiveMemory()
		}
		dirProgressBar.Finish()
		timeDelta("Create Google Drive directories")
	}

	fileProgressBar := pb.New64(nBytesToUpload).SetUnits(pb.U_BYTES)
	fileProgressBar.ShowBar = true
	fileProgressBar.Output = os.Stderr
	fileProgressBar.Prefix("Files: ")
	fileProgressBar.Start()

	// Sort the files by size, small to large.
	sort.Sort(LocalToRemoteBySize(fileMappings))

	// The two indices uploadFrontIndex and uploadBackIndex point to the
	// range of elements in the fileMappings array that haven't yet been
	// uploaded.
	uploadFrontIndex := 0
	uploadBackIndex := len(fileMappings) - 1

	// First, upload any large files that will use the resumable upload
	// protocol using a single thread; more threads here doesn't generally
	// help improve bandwidth utilizaiton and seems to make rate limit
	// errors from the Drive API more frequent...
	for ; uploadBackIndex >= 0; uploadBackIndex-- {
		if fileMappings[uploadBackIndex].LocalFileInfo.Size() < resumableUploadMinSize {
			break
		}

		fm := fileMappings[uploadBackIndex]
		if fm.LocalFileInfo.IsDir() {
			continue
		}

		if err := syncFileUp(fm, encrypt, existingFiles, fileProgressBar); err != nil {
			atomic.AddInt32(&nUploadErrors, 1)
			fmt.Fprintf(os.Stderr, "\nskicka: %s: %v\n", fm.LocalPath, err)
		}
		updateActiveMemory()
	}

	// Smaller files will be handled with multiple threads going at once;
	// doing so improves bandwidth utilization since round-trips to the
	// Drive APIs take a while.  (However, we don't want too have too many
	// workers; this would both lead to lots of 403 rate limit errors as
	// well as possibly increase memory use too much if we're uploading
	// lots of large files...)
	nWorkers := 4

	// Upload worker threads send a value over this channel when
	// they're done; the code that launches them waits for all of them
	// to do so before returning.
	doneChan := make(chan int)

	// Now that multiple threads are running, we need a mutex to protect
	// access to uploadFrontIndex and uploadBackIndex.
	var uploadIndexMutex sync.Mutex

	// All but one of the upload threads will grab files to upload starting
	// from the begining of the fileMappings array, thus doing the smallest
	// files first; one thread starts from the back of the array, doing the
	// largest files first.  In this way, the large files help saturate the
	// available upload bandwidth and hide the fixed overhead of creating
	// the smaller files.
	uploadWorker := func(startFromFront bool) {
		for {
			uploadIndexMutex.Lock()
			if uploadFrontIndex > uploadBackIndex {
				// All files have been uploaded.
				debug.Printf("All files uploaded [%d,%d]; exiting",
					uploadFrontIndex, uploadBackIndex)
				uploadIndexMutex.Unlock()
				doneChan <- 1
				break
			}

			// Get the index into fileMappings for the next file this
			// worker should upload.
			var index int
			if startFromFront {
				index = uploadFrontIndex
				uploadFrontIndex++
			} else {
				index = uploadBackIndex
				uploadBackIndex--
			}
			uploadIndexMutex.Unlock()

			fm := fileMappings[index]
			if fm.LocalFileInfo.IsDir() {
				continue
			}

			err = syncFileUp(fm, encrypt, existingFiles, fileProgressBar)
			if err != nil {
				atomic.AddInt32(&nUploadErrors, 1)
				fmt.Fprintf(os.Stderr, "\nskicka: %s: %v\n", fm.LocalPath, err)
			}
			updateActiveMemory()
		}
	}

	// Launch the workers.
	for i := 0; i < nWorkers; i++ {
		// All workers except the first one start from the front of
		// the array.
		go uploadWorker(i != 0)
	}

	// Wait for all of the workers to finish.
	for i := 0; i < nWorkers; i++ {
		<-doneChan
	}
	fileProgressBar.Finish()

	timeDelta("Sync files")

	if nUploadErrors == 0 {
		return nil
	}
	return fmt.Errorf("%d files not uploaded due to errors. "+
		"This is likely a transient failure; try uploading again", nUploadErrors)
}

func filterFilesToUpload(fileMappings []LocalToRemoteFileMapping,
	existingDriveFiles map[string]*drive.File,
	encrypt, ignoreTimes bool) ([]LocalToRemoteFileMapping, error) {

	// files to be uploaded are kept in this slice
	var toUpload []LocalToRemoteFileMapping

	for _, file := range fileMappings {
		driveFile, exists := existingDriveFiles[file.RemotePath]
		if !exists {
			toUpload = append(toUpload, file)
		} else {
			// The file already exists on Drive; just make sure it has all
			// of the properties that we expect.
			if err := createMissingProperties(driveFile, file.LocalFileInfo.Mode(),
				encrypt); err != nil {

				return nil, err
			}

			// Go ahead and update the file's permissions if they've changed
			if err := updatePermissions(driveFile, file.LocalFileInfo.Mode()); err != nil {
				return nil, err
			}

			if file.LocalFileInfo.IsDir() {
				// If it's a directory, once it's created and the permissions and times
				// are updated (if needed), we're all done.
				t, err := getModificationTime(driveFile)
				if err != nil {
					return nil, err
				}
				if !t.Equal(file.LocalFileInfo.ModTime()) {
					if err := updateModificationTime(driveFile, file.LocalFileInfo.ModTime()); err != nil {
						return nil, err
					}
				}
				continue
			}

			// Do superficial checking on the files
			metadataMatches, err := fileMetadataMatches(file.LocalFileInfo, encrypt, driveFile)

			if err != nil {
				return nil, err
			} else if metadataMatches && !ignoreTimes {
				continue
			}

			var iv []byte
			if encrypt {
				iv, err = getInitializationVector(driveFile)
				if err != nil {
					return nil, fmt.Errorf("unable to get IV: %v", err)
				}
			}

			// Check if the saved MD5 on Drive is the same when it's recomputed locally
			md5contents, err := localFileMD5Contents(file.LocalPath, encrypt, iv)
			if err != nil {
				return nil, err
			}

			contentsMatch := md5contents == driveFile.Md5Checksum
			if contentsMatch {
				// The timestamp of the local file is different, but the contents
				// are unchanged versus what's on Drive, so just update the
				// modified time on Drive so that we don't keep checking this
				// file.
				debug.Printf("contents match, timestamps do not")
				if err := updateModificationTime(driveFile, file.LocalFileInfo.ModTime()); err != nil {
					return nil, err
				}
			} else if metadataMatches == true {
				// We're running with -ignore-times, the modification times
				// matched, but the file contents were different. This is both
				// surprising and disturbing; it specifically suggests that
				// either the file contents were modified without the file's
				// modification time being updated, or that there was file
				// corruption of some sort. We'll be conservative and not clobber
				// the Drive file in case it was the latter.
				return nil, fmt.Errorf("has different contents versus Google " +
					"Drive, but doesn't have a newer timestamp. **Not updating" +
					"the file on Drive**. Run 'touch' to update the file" +
					"modification time and re-run skicka if you do want to" +
					"update the file.")
			} else {
				toUpload = append(toUpload, file)
			}
		}
	}

	return toUpload, nil
}

func compileUploadFileTree(localPath, driveRoot string, encrypt bool) ([]LocalToRemoteFileMapping, error) {
	// Walk the local directory hierarchy starting at 'localPath' and build
	// an array of files that may need to be synchronized.
	var fileMappings []LocalToRemoteFileMapping

	walkFuncCallback := func(path string, info os.FileInfo, patherr error) error {
		path = filepath.Clean(path)
		if patherr != nil {
			debug.Printf("%s: %v", path, patherr)
			return nil
		}

		// Check to see if the filename matches one of the regular
		// expressions of files to ignore.
		for _, re := range config.Upload.Ignored_Regexp {
			match, err := regexp.MatchString(re, path)
			if match == true {
				fmt.Printf("skicka: ignoring file %s, which "+
					"matches regexp \"%s\".\n", path, re)
				return nil
			}
			if err != nil {
				return err
			}
		}

		if (info.Mode() & os.ModeSymlink) != 0 {
			fmt.Printf("skicka: ignoring symlink \"%s\".\n", path)
			return nil
		}

		// Get the file's path relative to the base directory we're
		// uplaoding from.
		relPath, err := filepath.Rel(localPath, path)
		if err != nil {
			return err
		}
		drivePath := filepath.Clean(driveRoot + "/" + relPath)
		if info.IsDir() == false && encrypt == true {
			drivePath += encryptionSuffix
		}
		fileMappings = append(fileMappings, LocalToRemoteFileMapping{path, drivePath, info})
		return nil
	}

	err := filepath.Walk(localPath, walkFuncCallback)
	return fileMappings, err
}

///////////////////////////////////////////////////////////////////////////
// Downloading files and directory hierarchies from Google Drive

// If a file is encrypted, it should both have the initialization vector used
// to encrypt it stored as a Drive file property and have encryptionSuffix at the end
// of its filename. This function checks both of these and returns an error if
// these indicators are inconsistent; otherwise, it returns true/false
// accordingly.
func isEncrypted(file *drive.File) (bool, error) {
	if _, err := getProperty(file, "IV"); err == nil {
		if strings.HasSuffix(file.Title, encryptionSuffix) {
			return true, nil
		}
		return false, fmt.Errorf("has IV property but doesn't " +
			"end with .aes256 suffix")
	} else if strings.HasSuffix(file.Title, encryptionSuffix) {
		// This could actually happen with an interrupted upload
		// with 403 errors and the case where a file is created
		// even though a 403 happened, if we don't get to delete
		// the file before exiting...
		return false, fmt.Errorf("ends with .aes256 suffix but doesn't " +
			"have IV property")
	}
	return false, nil
}

// Checks to see if it's necessary to download the given *drive.File in order
// to create or update the corresponding local file.
func fileNeedsDownload(localPath string, drivePath string, driveFile *drive.File,
	ignoreTimes bool) (bool, error) {
	// See if the local file exists at all.
	stat, err := os.Stat(localPath)
	if err != nil {
		// The local file doesn't exist (probably).
		// TODO: confirm that's in fact the error...
		return true, nil
	}

	encrypt, err := isEncrypted(driveFile)
	if err != nil {
		return false, err
	}

	// Compare the local and Drive file sizes.
	diskSize := stat.Size()
	driveSize := driveFile.FileSize
	if encrypt {
		driveSize -= aes.BlockSize
	}
	if diskSize != driveSize {
		return true, nil
	}

	driveModificationTime, err := getModificationTime(driveFile)
	if err != nil {
		debug.Printf("unable to get modification time for %s: %v", drivePath, err)
		return true, nil
	}
	if ignoreTimes == false {
		if stat.ModTime().Equal(driveModificationTime) {
			return false, nil
		}
		if stat.ModTime().After(driveModificationTime) {
			fmt.Fprintf(os.Stderr, "skicka: warning: file %s is more "+
				"recent than %s on Google Drive. Skipping download.\n",
				localPath, drivePath)
			return false, nil
		}
	}

	// check MD5 checksums...
	var iv []byte
	if encrypt {
		iv, err = getInitializationVector(driveFile)
		if err != nil {
			return false, fmt.Errorf("unable to get IV: %v", err)
		}
	}

	md5contents, err := localFileMD5Contents(localPath, encrypt, iv)
	if err != nil {
		return true, err
	}

	if ignoreTimes && md5contents != driveFile.Md5Checksum &&
		stat.ModTime().After(driveModificationTime) == false {
		fmt.Fprintf(os.Stderr, "skicka: warning: %s is older than "+
			"file in Google Drive but file contents differ!\n",
			localPath)
	}

	return md5contents != driveFile.Md5Checksum, nil
}

// Create (or update the permissions) of the local directory corresponding to
// the given drive folder.
func syncFolderDown(localPath string, driveFilename string, driveFile *drive.File) error {
	permissions, err := getPermissions(driveFile)
	if err != nil {
		permissions = 0755
	}

	if stat, err := os.Stat(localPath); err == nil {
		// A file or directory already exists at localPath.
		if stat.IsDir() {
			err = os.Chmod(localPath, permissions)
		} else {
			return fmt.Errorf("%s: is a regular file", localPath)
		}
	} else {
		verbose.Printf("Creating directory %s for %s with permissions %#o",
			localPath, driveFilename, permissions)
		return os.Mkdir(localPath, permissions)
	}
	return nil
}

// Sync the given file from Google Drive to the local filesystem.
func downloadDriveFile(writer io.Writer, driveFile *drive.File) error {
	driveContentsReader, err := getDriveFileContentsReader(driveFile)
	if driveContentsReader != nil {
		defer driveContentsReader.Close()
	}
	if err != nil {
		return err
	}

	// Rate-limit the download, if required.
	var contentsReader io.Reader = driveContentsReader
	if config.Download.Bytes_per_second_limit > 0 {
		contentsReader = RateLimitedReader{R: driveContentsReader}
	}

	encrypted, err := isEncrypted(driveFile)
	if err != nil {
		return err
	}
	// Decrypt the contents, if they're encrypted.
	if encrypted {
		if key == nil {
			key, err = decryptEncryptionKey()
			if err != nil {
				return err
			}
		}

		// Read the initialization vector from the start of the file.
		iv := make([]byte, 16)
		n, err := contentsReader.Read(iv)
		if err != nil {
			return err
		}
		if n < aes.BlockSize {
			return fmt.Errorf("contents too short to hold IV: %d bytes", n)
		}
		// TODO: we should probably double check that the IV
		// matches the one in the Drive metadata and fail hard if not...
		contentsReader = makeDecryptionReader(key, iv, contentsReader)
	}

	contentsLength, err := io.Copy(writer, contentsReader)
	if err != nil {
		return err
	}

	atomic.AddInt64(&stats.DownloadBytes, contentsLength)
	atomic.AddInt64(&stats.DiskWriteBytes, contentsLength)
	atomic.AddInt64(&stats.LocalFilesUpdated, 1)
	return nil
}

// Download the full hierarchy of files from Google Drive starting at
// 'drivePath', recreating it at 'localPath'.
func syncHierarchyDown(drivePath string, localPath string,
	filesOnDrive map[string]*drive.File, ignoreTimes bool) error {
	var driveFilenames []string
	for name := range filesOnDrive {
		driveFilenames = append(driveFilenames, name)
	}
	sort.Strings(driveFilenames)

	// Both drivePath and localPath must be directories, or both must be files.
	if stat, err := os.Stat(localPath); err == nil && len(filesOnDrive) == 1 &&
		stat.IsDir() != isFolder(filesOnDrive[driveFilenames[0]]) {
		printErrorAndExit(fmt.Errorf("skicka: %s: remote and local must both be directory or both be files",
			localPath))
	}

	nDownloadErrors := int32(0)
	nBytesToDownload := int64(0)

	// 1) Download the folders, so that all of the directories we need have
	// been created before we start the files.
	// 2) Filter out everything that's not a file that needs to be downloaded
	for _, driveFilename := range driveFilenames {
		driveFile := filesOnDrive[driveFilename]
		filePath := localPath + "/" + driveFilename[len(drivePath):]

		if isFolder(driveFile) {
			err := syncFolderDown(filePath, driveFilename, driveFile)
			if err != nil {
				nDownloadErrors++
				fmt.Fprintf(os.Stderr, "skicka: %v\n", err)
			}
			delete(filesOnDrive, driveFilename)
		} else {
			needsDownload, err := fileNeedsDownload(filePath, driveFilename,
				driveFile, ignoreTimes)
			if err != nil {
				printErrorAndExit(fmt.Errorf("skicka: error determining if file %s should "+
					"be downloaded: %v", driveFilename, err))
			}
			if needsDownload {
				nBytesToDownload += driveFile.FileSize
			} else {
				delete(filesOnDrive, driveFilename)
			}
		}
	}

	// Kick off a background thread to periodically allow uploading
	// a bit more data.  This allowance is consumed by the
	// RateLimitedReader Read() function.
	launchBandwidthTask(config.Download.Bytes_per_second_limit)

	// Now do the files. Launch multiple workers to improve performance;
	// we're more likely to have some workers actively downloading file
	// contents while others are still getting ready, comparing files,
	// and making Drive API calls this way.
	nWorkers := 4
	indexChan := make(chan int)
	doneChan := make(chan int)
	var progressBar *pb.ProgressBar

	downloadWorker := func() {
		for {
			// Get the index into the driveFilenames[] array of the
			// file we should process next.
			index := <-indexChan
			if index < 0 {
				debug.Printf("Worker got index %d; exiting", index)
				doneChan <- 1
				break
			}

			driveFilename := driveFilenames[index]
			driveFile := filesOnDrive[driveFilename]
			filePath := localPath
			if len(driveFilename) > len(drivePath) {
				// If the Drive path is more than a single file.
				filePath += "/" + driveFilename[len(drivePath):]
			}

			writeCloser, err := createFileWriteCloser(filePath, driveFile)
			if err != nil {
				addErrorAndPrintMessage(&nDownloadErrors, "skicka: error creating file write closer.", err)
				continue
			}
			defer writeCloser.Close()

			multiwriter := io.MultiWriter(writeCloser, progressBar)

			if err := downloadDriveFile(multiwriter, driveFile); err != nil {
				addErrorAndPrintMessage(&nDownloadErrors, "skicka: error downloading drive file.", err)
				continue
			}
			if err := updateLocalFileProperties(filePath, driveFile); err != nil {
				addErrorAndPrintMessage(&nDownloadErrors, "skicka: error updating the local file.", err)
				continue
			}
			debug.Printf("Downloaded %d bytes for %s", driveFile.FileSize, filePath)
			verbose.Printf("Wrote %d bytes to %s", driveFile.FileSize, filePath)
			updateActiveMemory()
		}
	}

	progressBar = pb.New64(nBytesToDownload).SetUnits(pb.U_BYTES)
	progressBar.ShowBar = true
	progressBar.Output = os.Stderr
	if nBytesToDownload == 0 {
		fmt.Fprintf(os.Stderr, "Nothing to download\n")
		return nil
	}
	progressBar.Start()

	// Launch the workers.
	for i := 0; i < nWorkers; i++ {
		go downloadWorker()
	}
	// Give them the indices of the filenames of actual files (not
	// directories).
	for index, driveFilename := range driveFilenames {
		if filesOnDrive[driveFilename] != nil {
			indexChan <- index
		}
	}
	// Wrap up by sending "stop working" indices.
	for i := 0; i < nWorkers; i++ {
		indexChan <- -1
	}
	// And now wait for the workers to all return.
	for i := 0; i < nWorkers; i++ {
		<-doneChan
	}
	progressBar.Finish()

	if nDownloadErrors == 0 {
		return nil
	}
	return fmt.Errorf("%d files not downloaded due to errors", nDownloadErrors)
}

func addErrorAndPrintMessage(totalErrors *int32, message string, err error) {
	fmt.Fprintf(os.Stderr, message+" Error: %s\n", err)
	atomic.AddInt32(totalErrors, 1)
}

func printErrorAndExit(err error) {
	fmt.Fprintf(os.Stderr, "\r") // erase progress bar, if any
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func printUsageAndExit() {
	usage()
	os.Exit(1)
}

func createFileWriteCloser(localPath string, driveFile *drive.File) (io.WriteCloser, error) {
	encrypted, err := isEncrypted(driveFile)
	if err != nil {
		return nil, err
	}
	if encrypted {
		localPath = strings.TrimSuffix(localPath, encryptionSuffix)
	}

	// Create or overwrite the local file.
	f, err := os.Create(localPath)
	if err != nil {
		return nil, err
	}

	permissions, err := getPermissions(driveFile)
	if err != nil {
		permissions = 0644
	}
	f.Chmod(permissions)

	// Set the last access and modification time of the newly-created
	// file to match the modification time of the original file that was
	// uploaded to Google Drive.
	if modifiedTime, err := getModificationTime(driveFile); err == nil {
		return f, os.Chtimes(localPath, modifiedTime, modifiedTime)
	}
	return f, err
}

func updateLocalFileProperties(filepath string, file *drive.File) error {
	// make sure that the local permissions and modification
	// time match the corresponding values stored in Drive.
	modifiedTime, err := getModificationTime(file)
	if err != nil {
		return err
	}
	err = os.Chtimes(filepath, modifiedTime, modifiedTime)
	if err != nil {
		return err
	}
	permissions, err := getPermissions(file)
	if err != nil {
		permissions = 0644
	}
	if err := os.Chmod(filepath, permissions); err != nil {
		return err
	}
	return nil
}

///////////////////////////////////////////////////////////////////////////
// main (and its helpers)

// Create an empty configuration file for the user to use as a starting-point.
func createConfigFile(filename string) {
	contents := `; Default .skicka.config file. See 
; https://github.com/google/skicka/blob/master/README.md for more
; information about setting up skicka.
[google]
	clientid=YOUR_GOOGLE_APP_CLIENT_ID
	clientsecret=YOUR_GOOGLE_APP_SECRET
[encryption]
        ; Run 'skicka genkey' to generate an encyption key.
	;salt=
	;passphrase-hash=
	;encrypted-key=
	;encrypted-key-iv=
[upload]
	; You may want to specify regular expressions to match local filenames
	; that you want to be ignored by 'skicka upload'. Use one ignored-regexp
        ; line for each such regular expression.
	;ignored-regexp="\\.o$"
	;ignored-regexp=~$
	;
	; To limit upload bandwidth, you can set the maximum (average)
	; bytes per second that will be used for uploads
	;bytes-per-second-limit=524288  ; 512kB
`
	// Don't overwrite an already-existing configuration file.
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		err := ioutil.WriteFile(filename, []byte(contents), 0600)
		if err != nil {
			printErrorAndExit(fmt.Errorf("skicka: unable to create "+
				"configuration file %s: %v", filename, err))
		}
		fmt.Printf("skicka: created configuration file %s.\n", filename)
	} else {
		printErrorAndExit(fmt.Errorf("skicka: %s: file already exists; "+
			"leaving it alone.", filename))
	}
}

func checkEncryptionConfig(value string, name string, bytes int) int {
	if value == "" {
		return 0
	}
	if num, err := hex.DecodeString(value); err != nil || len(num) != bytes {
		fmt.Fprintf(os.Stderr, "skicka: missing or invalid "+
			"[encryption]/%s value (expecting %d hex "+
			"characters).\n", name, 2*bytes)
		return 1
	}
	return 0
}

// Check that the configuration read from the config file isn't obviously
// missing needed entries so that we can give better error messages at startup
// while folks are first getting things setup.
func checkConfigValidity() {
	nerrs := 0
	if config.Google.ClientId == "" ||
		config.Google.ClientId == "YOUR_GOOGLE_APP_CLIENT_ID" {
		fmt.Fprintf(os.Stderr, "skicka: missing [google]/clientid in "+
			"configuration file.\n")
		nerrs++
	}
	if config.Google.ClientSecret == "" ||
		config.Google.ClientSecret == "YOUR_GOOGLE_APP_SECRET" {
		fmt.Fprintf(os.Stderr, "skicka: missing [google]/clientsecret in "+
			"configuration file.\n")
		nerrs++
	}

	// It's ok if the encryption stuff isn't present (if encryption
	// isn't being used), but if it is present, it must be valid...
	nerrs += checkEncryptionConfig(config.Encryption.Salt, "salt", 32)
	nerrs += checkEncryptionConfig(config.Encryption.Passphrase_hash,
		"passphrase-hash", 32)
	nerrs += checkEncryptionConfig(config.Encryption.Encrypted_key,
		"encrypted-key", 32)
	nerrs += checkEncryptionConfig(config.Encryption.Encrypted_key_iv,
		"encrypted-key-iv", 16)

	if nerrs > 0 {
		os.Exit(1)
	}
}

func readConfigFile(filename string) {
	filename, err := tildeExpand(filename)
	if err != nil {
		printErrorAndExit(fmt.Errorf("skicka: %s: error expanding configuration "+
			"file path: %v", filename, err))
	}

	if info, err := os.Stat(filename); err != nil {
		printErrorAndExit(fmt.Errorf("skicka: %v", err))
	} else if goperms := info.Mode() & ((1 << 6) - 1); goperms != 0 {
		printErrorAndExit(fmt.Errorf("skicka: %s: permissions of configuration file "+
			"allow group/other access. Your secrets are at risk.",
			filename))
	}

	err = gcfg.ReadFileInto(&config, filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skicka: %s: %v\n", filename, err)
		printErrorAndExit(fmt.Errorf("skicka: you may want to run \"skicka " +
			"init\" to create an initial configuration file."))
	}
	checkConfigValidity()
}

func usage() {
	fmt.Printf(
		`skicka is a tool for working with files and folders on Google Drive.
See http://github.com/google/skicka/README.md for information about getting started.

usage: skicka [common options] <command> [command options]

Commands and their options are:
  cat        Print the contents of the Google Drive file to standard output.
             Arguments: <gdrive path>

  download   Recursively download all files from a Google Drive folder to a
             local directory. If a local file already exists and has the same
             contents as the corresponding Google Drive file, the download is
             skipped.
             Arguments: [-ignore-times] <drive path> <local directory> 

  du         Print the space used by the Google Drive folder and its children.
             Arguments: <drive path>

  genkey     Generate a new key for encrypting files.

  init       Create an initial ~/.skicka.config configuration file. (You
             will need to edit it before using skicka; see comments in the
             configuration file for details.)

  ls         List the files and directories in the given Google Drive folder.
             Arguments: [-l, -ll, -r] [drive path],
             where -l and -ll specify long (including sizes and update times)
             and really long output (also including MD5 checksums), respectively.
             The -r argument causes ls to recursively list all files in the
             hierarchy rooted at the base directory.

  mkdir      Create a new directory (folder) at the given Google Drive path.
             Arguments: [-p] <drive path>,
             where intermediate directories in the path are created if -p is
             specified.

  rm	     Remove a file or directory at the given Google Drive path.
             Arguments: [-r, -s] <drive path>,
             where files and directories are recursively removed if -r is specified
             and the google drive trash is skipped if -s is specified. The default 
             behavior is to fail if the drive path specified is a directory and -r is
             not specified, and to send files to the trash instead of permanently
             deleting them.

  upload     Uploads all files in the local directory and its children to the
             given Google Drive path. Skips files that have already been
             uploaded.
             Arguments: [-ignore-times] [-encrypt] <local directory> <drive path>

Options valid for both "upload" and "download":
  -ignore-times    Normally, skicka assumes that if the timestamp of a local file
                   matches the timestamp of the file on Drive and the files have
                   the same size, then it isn't necessary to confirm that the
                   file contents match. The -ignore-times flag can be used to
                   force checking file contents in this case.

General options valid for all commands:
  -config=<filename>     Specify a configuration file. Default: ~/.skicka.config.
  -debug                 Enable debugging output.
  -help                  Print this help message.
  -tokencache=<filename> OAuth2 token cache file. Default: ~/.skicka.tokencache.json.
  -verbose               Enable verbose output.
`)
}

func du(args []string) {

	if len(args) != 1 {
		printUsageAndExit()
	}
	drivePath := filepath.Clean(args[0])

	recursive := true
	includeBase := false
	mustExist := true
	existingFiles := getFilesAtRemotePath(drivePath, recursive, includeBase,
		mustExist)

	// Accumulate the size in bytes of each folder in the hierarchy
	folderSize := make(map[string]int64)
	var dirNames []string
	totalSize := int64(0)
	for name, f := range existingFiles {
		if isFolder(f) {
			dirNames = append(dirNames, name)
		} else {
			totalSize += f.FileSize
			dirName := filepath.Clean(filepath.Dir(name))
			for ; dirName != "/"; dirName = filepath.Dir(dirName) {
				folderSize[dirName] += f.FileSize
			}
			folderSize["/"] += f.FileSize
		}
	}

	// Print output
	sort.Strings(dirNames)
	for _, d := range dirNames {
		fmt.Printf("%s  %s\n", fmtbytes(folderSize[d], true), d)
	}
	fmt.Printf("%s  %s\n", fmtbytes(totalSize, true), drivePath)
}

func cat(args []string) {
	if len(args) != 1 {
		printUsageAndExit()
	}
	filename := filepath.Clean(args[0])

	file, err := getDriveFile(filename)
	timeDelta("Get file descriptors from Google Drive")
	if err != nil {
		printErrorAndExit(err)
	}
	if isFolder(file) {
		printErrorAndExit(fmt.Errorf("skicka: %s: is a directory", filename))
	}

	contentsReader, err := getDriveFileContentsReader(file)
	if contentsReader != nil {
		defer contentsReader.Close()
	}
	if err != nil {
		printErrorAndExit(fmt.Errorf("skicka: %s: %v", filename, err))
	}

	_, err = io.Copy(os.Stdout, contentsReader)
	if err != nil {
		printErrorAndExit(fmt.Errorf("skicka: %s: %v", filename, err))
	}
}

func mkdir(args []string) {
	makeIntermediate := false

	i := 0
	for ; i+1 < len(args); i++ {
		if args[i] == "-p" {
			makeIntermediate = true
		} else {
			printUsageAndExit()
		}
	}
	drivePath := filepath.Clean(args[i])

	parent, err := getFileById("root")
	if err != nil {
		printErrorAndExit(fmt.Errorf("unable to get Drive root directory: %v", err))
	}

	dirs := strings.Split(drivePath, "/")
	nDirs := len(dirs)
	pathSoFar := ""
	// Walk through the directories in the path in turn.
	for index, dir := range dirs {
		if dir == "" {
			// The first string in the split is "" if the
			// path starts with a '/'.
			continue
		}
		pathSoFar += "/" + dir

		// Get the Drive File file for our current point in the path.
		query := fmt.Sprintf("title='%s' and '%s' in parents and "+
			"trashed=false", dir, parent.Id)
		files := runDriveQuery(query)

		if len(files) > 1 {
			printErrorAndExit(fmt.Errorf("skicka: %s: multiple files with "+
				"this name", pathSoFar))
		}

		if len(files) == 0 {
			// File not found; create the folder if we're at the last
			// directory in the provided path or if -p was specified.
			// Otherwise, error time.
			if index+1 == nDirs || makeIntermediate {
				parent, err = createDriveFolder(dir, 0755, time.Now(), parent)
				debug.Printf("Creating folder %s", pathSoFar)
				if err != nil {
					printErrorAndExit(fmt.Errorf("skicka: %s: %v",
						pathSoFar, err))
				}
			} else {
				printErrorAndExit(fmt.Errorf("skicka: %s: no such "+
					"directory", pathSoFar))
			}
		} else {
			// Found it; if it's a folder this is good, unless it's
			// the folder we were supposed to be creating.
			if index+1 == nDirs && !makeIntermediate {
				printErrorAndExit(fmt.Errorf("skicka: %s: already exists",
					pathSoFar))
			} else if !isFolder(files[0]) {
				printErrorAndExit(fmt.Errorf("skicka: %s: not a folder",
					pathSoFar))
			} else {
				parent = files[0]
			}
		}
	}
}

func getPermissionsAsString(driveFile *drive.File) (string, error) {
	var str string
	if isFolder(driveFile) {
		str = "d"
	} else {
		str = "-"
	}

	perm, err := getPermissions(driveFile)
	if err != nil {
		str += "?????????"
	} else {
		rwx := "rwx"
		for i := 0; i < 9; i++ {
			if perm&(1<<(8-uint(i))) != 0 {
				str += string(rwx[i%3])
			} else {
				str += "-"
			}
		}
	}
	return str, nil
}

var rmSyntaxError CommandSyntaxError = CommandSyntaxError{
	Cmd: "rm",
	Msg: "drive path cannot be empty.\n" +
		"Usage: rm [-r, -s] drive path",
}

type driveQueryer interface {
	getDriveFile() (*drive.File, error)
	drivePath() string
}

type driveDeleter interface {
	deleteDriveFile() error
	trashDriveFile() (*drive.File, error)
	isSkipTrash() bool
}

type RmCommand struct {
	recursive     bool
	skipTrash     bool
	path          string
	svc           *drive.Service
	queryService  driveQueryer
	deleteService driveDeleter
	driveFile     *drive.File
}

func (rmCmd *RmCommand) getDriveFile() (*drive.File, error) {
	driveFile, err := getDriveFile(rmCmd.path)
	if err == nil {
		rmCmd.driveFile = driveFile
	}
	return driveFile, err
}

func (rmCmd *RmCommand) deleteDriveFile() error {
	debug.Printf("Deleting file %s (id %s)", rmCmd.path, rmCmd.driveFile.Id)
	return rmCmd.svc.Files.Delete(rmCmd.driveFile.Id).Do()
}

func (rmCmd *RmCommand) trashDriveFile() (*drive.File, error) {
	debug.Printf("Trashing file %s (id %s)", rmCmd.path, rmCmd.driveFile.Id)
	return rmCmd.svc.Files.Trash(rmCmd.driveFile.Id).Do()
}

func (rmCmd *RmCommand) drivePath() string {
	return rmCmd.path
}

func (rmCmd *RmCommand) isSkipTrash() bool {
	return rmCmd.skipTrash
}

func rm(args []string) {
	recursive, skipTrash := false, false
	var drivePath string

	for _, arg := range args {
		switch {
		case arg == "-r":
			recursive = true
		case arg == "-s":
			skipTrash = true
		case drivePath == "":
			drivePath = arg
		default:
			printErrorAndExit(rmSyntaxError)
		}
	}

	rmCmd := &RmCommand{
		recursive: recursive,
		skipTrash: skipTrash,
		path:      drivePath,
		svc:       drivesvc,
	}

	if err := checkRmPossible(rmCmd, recursive); err != nil {
		if _, ok := err.(fileNotFoundError); ok {
			// if there's an encrypted version on drive, let the user know and exit
			oldPath := rmCmd.path
			rmCmd.path += encryptionSuffix
			if err := checkRmPossible(rmCmd, recursive); err == nil {
				printErrorAndExit(fmt.Errorf("skicka rm: Found no file with path %s, but found encrypted version with path %s.\n"+
					"If you would like to rm the encrypted version, re-run the command adding the %s extension onto the path.",
					oldPath, rmCmd.path, encryptionSuffix))
			}
		}
		printErrorAndExit(err)
	}

	for nTries := 5; ; nTries++ {
		err := deleteDriveFile(rmCmd)
		if err == nil {
			return
		}
		if err = tryToHandleDriveAPIError(err, nTries); err != nil {
			printErrorAndExit(err)
		}
	}
}

func deleteDriveFile(deleter driveDeleter) error {
	skipTrash := deleter.isSkipTrash()
	if skipTrash {
		// do delete
		return deleter.deleteDriveFile()
	} else {
		// do trash
		_, err := deleter.trashDriveFile()
		return err
	}
}

func checkRmPossible(queryer driveQueryer, recursive bool) error {
	if queryer.drivePath() == "" {
		return rmSyntaxError
	}

	invokingCmd := "skicka rm"

	driveFile, err := queryer.getDriveFile()
	if err != nil {
		switch err.(type) {
		case fileNotFoundError:
			return fileNotFoundError{
				path:        queryer.drivePath(),
				invokingCmd: invokingCmd,
			}
		default:
			return err
		}
	}

	if !recursive && isFolder(driveFile) {
		return removeDirectoryError{
			path:        queryer.drivePath(),
			invokingCmd: invokingCmd,
		}
	}

	return nil
}

func ls(args []string) {
	long := false
	longlong := false
	recursive := false
	var drivePath string
	for _, value := range args {
		switch {
		case value == "-l":
			long = true
		case value == "-ll":
			longlong = true
		case value == "-r":
			recursive = true
		case drivePath == "":
			drivePath = value
		default:
			printUsageAndExit()
		}
	}

	if drivePath == "" {
		drivePath = "/"
	}
	drivePath = filepath.Clean(drivePath)

	includeBase := false
	mustExist := true
	existingFiles := getFilesAtRemotePath(drivePath, recursive, includeBase,
		mustExist)

	var filenames []string
	for f := range existingFiles {
		filenames = append(filenames, f)
	}
	sort.Strings(filenames)

	for _, f := range filenames {
		file := existingFiles[f]
		printFilename := f
		if !recursive {
			printFilename = filepath.Base(f)
		}
		if isFolder(file) {
			printFilename += "/"
		}
		if long || longlong {
			synctime, _ := getModificationTime(file)
			permString, _ := getPermissionsAsString(file)
			if longlong {
				md5 := file.Md5Checksum
				if len(md5) != 32 {
					md5 = "--------------------------------"
				}
				fmt.Printf("%s  %s  %s  %s  %s\n", permString,
					fmtbytes(file.FileSize, true), md5,
					synctime.Format(time.ANSIC), printFilename)
				if debug {
					fmt.Printf("\t[ ")
					for _, prop := range file.Properties {
						fmt.Printf("%s: %s, ", prop.Key,
							prop.Value)
					}
					fmt.Printf("id: %s ]\n", file.Id)
				}
			} else {
				fmt.Printf("%s  %s  %s  %s\n", permString,
					fmtbytes(file.FileSize, true),
					synctime.Format(time.ANSIC), printFilename)
			}
		} else {
			fmt.Printf("%s\n", printFilename)
		}
	}
}

func upload(args []string) {
	ignoreTimes := false
	encrypt := false

	if len(args) < 2 {
		printUsageAndExit()
	}

	i := 0
	for ; i+2 < len(args); i++ {
		switch args[i] {
		case "-ignore-times":
			ignoreTimes = true
		case "-encrypt":
			encrypt = true
		default:
			printUsageAndExit()
		}
	}

	localPath := filepath.Clean(args[i])
	drivePath := filepath.Clean(args[i+1])

	// Make sure localPath exists and is a directory.
	if _, err := os.Stat(localPath); err != nil {
		printErrorAndExit(fmt.Errorf("skicka: %v", err))
	}

	recursive := true
	includeBase := true
	mustExist := false
	fmt.Fprintf(os.Stderr, "skicka: Getting list of files to upload... ")
	existingFiles := getFilesAtRemotePath(drivePath, recursive, includeBase,
		mustExist)
	fmt.Fprintf(os.Stderr, "Done.\n")

	syncStartTime = time.Now()
	err := syncHierarchyUp(localPath, drivePath, existingFiles, encrypt,
		ignoreTimes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skicka: error uploading %s: %v\n",
			localPath, err)
	}

	printFinalStats()
	if err != nil {
		os.Exit(1)
	}
}

func download(args []string) {
	if len(args) < 2 {
		printUsageAndExit()
	}

	ignoreTimes := false
	i := 0
	for ; i+2 < len(args); i++ {
		switch args[i] {
		case "-ignore-times":
			ignoreTimes = true
		default:
			printUsageAndExit()
		}
	}

	drivePath := filepath.Clean(args[i])
	localPath := filepath.Clean(args[i+1])

	recursive := true
	includeBase := true
	mustExist := true
	fmt.Fprintf(os.Stderr, "skicka: Getting list of files to download... ")
	existingFiles := getFilesAtRemotePath(drivePath, recursive, includeBase,
		mustExist)
	fmt.Fprintf(os.Stderr, "Done. Starting download.\n")

	syncStartTime = time.Now()
	err := syncHierarchyDown(drivePath, localPath, existingFiles, ignoreTimes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skicka: error downloading %s: %v\n",
			localPath, err)
	}

	printFinalStats()
	if err != nil {
		os.Exit(1)
	}
}

func main() {
	cachefile := flag.String("cache", "~/.skicka.tokencache.json",
		"OAuth2 token cache file")
	configFilename := flag.String("config", "~/.skicka.config",
		"Configuration file")
	vb := flag.Bool("verbose", false, "Enable verbose output")
	dbg := flag.Bool("debug", false, "Enable debugging output")
	flag.Usage = usage
	flag.Parse()

	if len(flag.Args()) == 0 {
		printUsageAndExit()
	}

	verbose = debugging(*vb || *dbg)
	debug = debugging(*dbg)

	var err error
	*configFilename, err = tildeExpand(*configFilename)
	if err != nil {
		printErrorAndExit(fmt.Errorf("skicka: %s: error expanding "+
			"config path: %v", *cachefile, err))
	}

	cmd := flag.Arg(0)
	// Commands that don't need the config file to be read or to use
	// the cached OAuth2 token.
	switch cmd {
	case "genkey":
		generateKey()
		return
	case "init":
		createConfigFile(*configFilename)
		return
	case "help":
		usage()
		return
	}

	*cachefile, err = tildeExpand(*cachefile)
	if err != nil {
		printErrorAndExit(fmt.Errorf("skicka: %s: error expanding "+
			"cachefile path: %v", *cachefile, err))
	}

	readConfigFile(*configFilename)

	err = createDriveClient(config.Google.ClientId, config.Google.ClientSecret,
		*cachefile)
	if err != nil {
		printErrorAndExit(fmt.Errorf("skicka: error creating Google Drive "+
			"client: %v", err))
	}

	args := flag.Args()[1:]

	switch cmd {
	case "du":
		du(args)
	case "cat":
		cat(args)
	case "ls":
		ls(args)
	case "mkdir":
		mkdir(args)
	case "upload":
		upload(args)
	case "download":
		download(args)
	case "rm":
		rm(args)
	default:
		printUsageAndExit()
	}
}
