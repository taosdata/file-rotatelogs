// package rotatelogs is a port of File-RotateLogs from Perl
// (https://metacpan.org/release/File-RotateLogs), and it allows
// you to automatically rotate output files when you write to them
// according to the filename pattern that you can specify.
package rotatelogs

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lestrrat-go/strftime"
	"github.com/pkg/errors"
	"github.com/taosdata/file-rotatelogs/v2/internal/fileutil"
)

const CheckDiskSizeInterval = 25 * time.Second

func (c clockFn) Now() time.Time {
	return c()
}

// New creates a new RotateLogs object. A log filename pattern
// must be passed. Optional `Option` parameters may be passed
func New(p string, options ...Option) (*RotateLogs, error) {
	logPath := filepath.Dir(p)
	isRootDir := isRoot(logPath)
	if isRootDir {
		return nil, errors.New("log path can't be root directory")
	}
	globPattern := p + "*"
	for _, re := range patternConversionRegexps {
		globPattern = re.ReplaceAllString(globPattern, "*")
	}

	pattern, err := strftime.New(p)
	if err != nil {
		return nil, errors.Wrap(err, `invalid strftime pattern`)
	}
	var clock Clock = Local
	rotationTime := 24 * time.Hour
	var rotationSize int64 = 1 * 1024 * 1024 * 1024 // 1GB
	var rotationCount uint
	var linkName string
	var maxAge time.Duration
	var handler Handler
	var forceNewFile bool
	var reservedDiskSize int64
	var compress bool
	var cleanLockName = filepath.Join(logPath, ".rotate_clean_lock")

	for _, o := range options {
		switch o.Name() {
		case optkeyClock:
			clock = o.Value().(Clock)
		case optkeyLinkName:
			linkName = o.Value().(string)
		case optkeyMaxAge:
			maxAge = o.Value().(time.Duration)
			if maxAge < 0 {
				maxAge = 0
			}
		case optkeyRotationTime:
			rotationTime = o.Value().(time.Duration)
			if rotationTime < 0 {
				rotationTime = 0
			}
		case optkeyRotationSize:
			rotationSize = o.Value().(int64)
			if rotationSize < 0 {
				rotationSize = 0
			}
		case optkeyRotationCount:
			rotationCount = o.Value().(uint)
		case optkeyHandler:
			handler = o.Value().(Handler)
		case optkeyForceNewFile:
			forceNewFile = true
		case optKeyReservedDiskSize:
			reservedDiskSize = o.Value().(int64)
		case optKeyGlobPattern:
			globPattern = o.Value().(string)
		case optKeyCompress:
			compress = o.Value().(bool)
		case optKeyCleanLockFile:
			cleanLockName = o.Value().(string)
		}
	}

	rl := &RotateLogs{
		clock:            clock,
		eventHandler:     handler,
		globPattern:      globPattern,
		linkName:         linkName,
		maxAge:           maxAge,
		pattern:          pattern,
		rotationTime:     rotationTime,
		rotationSize:     rotationSize,
		rotationCount:    rotationCount,
		forceNewFile:     forceNewFile,
		logPath:          logPath,
		reservedDiskSize: reservedDiskSize,
		compress:         compress,
		rotateCleanChan:  make(chan struct{}, 5),
		exitChan:         make(chan struct{}),
		done:             make(chan struct{}),
		cleanLockName:    cleanLockName,
	}
	if reservedDiskSize > 0 {
		err = os.MkdirAll(rl.logPath, 0755)
		if err != nil {
			return nil, errors.Wrap(err, fmt.Sprintf("make dir for %s fail", rl.logPath))
		}
		_, avail, err := GetDiskSize(rl.logPath)
		if err != nil {
			return nil, errors.Wrap(err, `failed to get disk size`)
		}
		rl.availDiskSize = int64(avail)
	}
	go func() { rl.waitSignal() }()
	return rl, nil
}

func isRoot(path string) bool {
	cleanPath := filepath.Clean(path)
	return cleanPath == "/" || cleanPath == "C:\\"
}

func (rl *RotateLogs) waitSignal() {
	ticker := time.NewTicker(CheckDiskSizeInterval)
	go func() {
		for {
			select {
			case <-ticker.C:
				_, avail, err := GetDiskSize(rl.logPath)
				if err == nil {
					atomic.StoreInt64(&rl.availDiskSize, int64(avail))
				}
			case <-rl.rotateCleanChan:
				rl.rotateClean()
			case <-rl.exitChan:
				ticker.Stop()
				close(rl.done)
				return
			}
		}
	}()
}

// Write satisfies the io.Writer interface. It writes to the
// appropriate file handle that is currently being used.
// If we have reached rotation time, the target file gets
// automatically rotated, and also purged if necessary.
func (rl *RotateLogs) Write(p []byte) (n int, err error) {
	// Guard against concurrent writes
	rl.mutex.Lock()
	defer rl.mutex.Unlock()
	if !rl.diskSpaceEnough() {
		return 0, errors.New("disk space is not enough")
	}
	out, err := rl.getWriterNolock(false, false)
	if err != nil {
		return 0, errors.Wrap(err, `failed to acquite target io.Writer`)
	}

	return out.Write(p)
}

func (rl *RotateLogs) diskSpaceEnough() bool {
	if rl.reservedDiskSize <= 0 {
		return true
	}
	return atomic.LoadInt64(&rl.availDiskSize) > rl.reservedDiskSize
}

// must be locked during this operation
func (rl *RotateLogs) getWriterNolock(bailOnRotateFail, useGenerationalNames bool) (*os.File, error) {
	generation := rl.generation
	previousFn := rl.curFn

	// This filename contains the name of the "NEW" filename
	// to log to, which may be newer than rl.currentFilename
	baseFn := fileutil.GenerateFn(rl.pattern, rl.clock, rl.rotationTime)
	var forceNewFile bool
	filename := baseFn
	sizeRotation := false
	fi, err := os.Stat(rl.curFn)
	if err == nil && rl.rotationSize > 0 && rl.rotationSize <= fi.Size() {
		forceNewFile = true
		sizeRotation = true
	}
	if rl.curBaseFn == "" {
		// first start, check the generation number
		var latestLogFile os.DirEntry
		generation, latestLogFile, err = getLatestLogFile(baseFn)
		if latestLogFile != nil {
			// end with gz means it is compressed, need to force new file
			if strings.HasSuffix(latestLogFile.Name(), ".gz") {
				forceNewFile = true
				generation += 1
			} else {
				if rl.forceNewFile {
					forceNewFile = true
					generation += 1
				} else {
					// check if the latest log file is full
					info, err := latestLogFile.Info()
					if err == nil && rl.rotationSize > 0 && rl.rotationSize > info.Size() {
						// use latestLogFile as current file
						filename = filepath.Join(rl.logPath, latestLogFile.Name())
					} else {
						// ignore error, just force new file
						forceNewFile = true
						generation += 1
					}
				}
			}
		} else {
			// no log file, just use baseFn
			generation = 0
			filename = baseFn
		}

	} else if baseFn != rl.curBaseFn {
		generation = 0
		// The base filename has changed. This means we need to create a new file
		forceNewFile = true
	} else {
		if !useGenerationalNames && !sizeRotation {
			// nothing to do
			return rl.outFh, nil
		}
		forceNewFile = true
		generation++
	}

	var fh *os.File
	var lockFh *os.File
	lockfileName := filename + `_lock`
	if !forceNewFile {
		// Try to open the file and lock it, if we can't, then we need to create a new file
		fh, lockFh, err = createLogFileAndLock(filename, lockfileName, true)
		if err != nil {
			forceNewFile = true
			generation += 1
		}
	}
	if forceNewFile {
		// A new file has been requested. Instead of just using the
		// regular strftime pattern, we create a new file name using
		// generational names such as "foo.1", "foo.2", "foo.3", etc
		var name string
		tryTimes := 0
		for {
			if tryTimes > 100 {
				return nil, errors.New("try create new files too many times")
			}
			if generation == 0 {
				name = baseFn
			} else {
				name = fmt.Sprintf("%s.%d", baseFn, generation)
			}
			lockfileName = name + `_lock`
			fh, lockFh, err = createLogFileAndLock(name, lockfileName, false)
			if err != nil {
				tryTimes += 1
				generation += 1
				continue
			}
			filename = name
			break
		}
	}
	if rl.lockFh != nil {
		UnlockFile(rl.lockFh)
		rl.lockFh.Close()
		rl.removeFile(rl.lockFilename)
	}
	if rl.outFh != nil {
		UnlockFile(rl.outFh)
		rl.outFh.Close()
	}
	rl.lockFh = lockFh
	rl.outFh = fh
	rl.curBaseFn = baseFn
	rl.curFn = filename
	rl.generation = generation
	rl.lockFilename = lockfileName
	// make link and rotate
	if err := rl.rotateNolock(filename); err != nil {
		err = errors.Wrap(err, "failed to rotate")
		if bailOnRotateFail {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "%s\n", err.Error())
	}

	if h := rl.eventHandler; h != nil {
		go h.Handle(&FileRotatedEvent{
			prev:    previousFn,
			current: filename,
		})
	}

	return fh, nil
}

func createLogFileAndLock(filename string, lockFilename string, appendFile bool) (*os.File, *os.File, error) {
	// Try to open the file and lock it, if we can't, then we need to create a new file
	logFileHandle, err := fileutil.CreateFile(filename, appendFile)
	if err != nil {
		return nil, nil, err
	}
	// check file in use
	err = LockFile(logFileHandle)
	if err != nil {
		logFileHandle.Close()
		return nil, nil, err
	}
	// create lock file and lock it
	// write only open file, avoid nfs file system lock file failed
	lockHandle, err := os.OpenFile(lockFilename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		UnlockFile(logFileHandle)
		logFileHandle.Close()
		return nil, nil, err
	}
	err = LockFile(lockHandle)
	if err != nil {
		// lock lockfile failed
		UnlockFile(logFileHandle)
		logFileHandle.Close()
		lockHandle.Close()
		return nil, nil, err
	}
	// unlock log file
	UnlockFile(logFileHandle)
	return logFileHandle, lockHandle, nil
}

func getLatestLogFile(baseFn string) (int, os.DirEntry, error) {
	dir, file := filepath.Split(baseFn)
	pattern := regexp.QuoteMeta(file) + `.(\d+)`
	re := regexp.MustCompile(pattern)
	files, err := os.ReadDir(dir)
	if err != nil {
		return 0, nil, errors.Wrap(err, `failed to read directory`)
	}
	generation := 0
	var latestLogFile os.DirEntry
	for _, f := range files {
		fName := f.Name()
		if strings.HasSuffix(fName, "_lock") || strings.HasSuffix(fName, "_symlink") || strings.HasSuffix(fName, ".tmp") {
			continue
		}
		if fName == file {
			latestLogFile = f
			continue
		}
		matches := re.FindStringSubmatch(fName)
		if len(matches) > 1 {
			num, err := strconv.Atoi(matches[1])
			if err == nil && num > generation {
				generation = num
				latestLogFile = f
			}
		}
	}
	return generation, latestLogFile, nil
}

// CurrentFileName returns the current file name that
// the RotateLogs object is writing to
func (rl *RotateLogs) CurrentFileName() string {
	rl.mutex.RLock()
	defer rl.mutex.RUnlock()

	return rl.curFn
}

var patternConversionRegexps = []*regexp.Regexp{
	regexp.MustCompile(`%[%+A-Za-z]`),
	regexp.MustCompile(`\*+`),
}

// Rotate forcefully rotates the log files. If the generated file name
// clash because file already exists, a numeric suffix of the form
// ".1", ".2", ".3" and so forth are appended to the end of the log file
//
// Thie method can be used in conjunction with a signal handler so to
// emulate servers that generate new log files when they receive a
// SIGHUP
func (rl *RotateLogs) Rotate() error {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()
	_, err := rl.getWriterNolock(true, true)

	return err
}

func (rl *RotateLogs) rotateNolock(filename string) error {
	if rl.linkName != "" {
		tmpLinkName := filename + `_symlink`

		// Change how the link name is generated based on where the
		// target location is. if the location is directly underneath
		// the main filename's parent directory, then we create a
		// symlink with a relative path
		linkDest := filename
		linkDir := filepath.Dir(rl.linkName)

		baseDir := filepath.Dir(filename)
		if strings.Contains(rl.linkName, baseDir) {
			tmp, err := filepath.Rel(linkDir, filename)
			if err != nil {
				return errors.Wrapf(err, `failed to evaluate relative path from %#v to %#v`, baseDir, rl.linkName)
			}

			linkDest = tmp
		}

		if err := os.Symlink(linkDest, tmpLinkName); err != nil {
			return errors.Wrap(err, `failed to create new symlink`)
		}

		// the directory where rl.linkName should be created must exist
		_, err := os.Stat(linkDir)
		if err != nil { // Assume err != nil means the directory doesn't exist
			if err := os.MkdirAll(linkDir, 0755); err != nil {
				return errors.Wrapf(err, `failed to create directory %s`, linkDir)
			}
		}

		if err := os.Rename(tmpLinkName, rl.linkName); err != nil {
			return errors.Wrap(err, `failed to rename new symlink`)
		}
	}

	select {
	case rl.rotateCleanChan <- struct{}{}:
	default:
	}
	return nil
}

func (rl *RotateLogs) rotateClean() error {
	// open lock file
	// write only open file, avoid nfs file system lock file failed
	cleanLockFile, err := os.OpenFile(rl.cleanLockName, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return errors.Wrap(err, `failed to acquire clean lock`)
	}
	getLocked := false
	defer func() {
		cleanLockFile.Close()
		// if you get locked, remove lock file after clean
		if getLocked {
			rl.removeFile(rl.cleanLockName)
		}
	}()
	// lock file
	err = LockFile(cleanLockFile)
	if err != nil {
		return errors.Wrap(err, `failed to lock clean lock file`)
	}
	getLocked = true

	// get log files with glob pattern
	matches, err := filepath.Glob(rl.globPattern)
	if err != nil {
		return err
	}

	cutoff := time.Now().Add(-1 * rl.maxAge)
	var toCompress []string
	totalFiles := make([]os.FileInfo, 0, len(matches))
	outDateFiles := make([]os.FileInfo, 0, len(matches))
	m := make(map[os.FileInfo]string)
	for _, p := range matches {
		// Ignore lock files and tmp files
		if strings.HasSuffix(p, "_lock") || strings.HasSuffix(p, "_symlink") || strings.HasSuffix(p, ".tmp") {
			continue
		}
		// Ignore files that are not in the same directory as the log file
		if filepath.Dir(p) != rl.logPath {
			continue
		}
		if rl.linkName != "" && p == rl.linkName {
			continue
		}

		fi, err := os.Stat(p)
		if err != nil {
			continue
		}

		fl, err := os.Lstat(p)
		if err != nil {
			continue
		}
		toCompress = append(toCompress, p)
		if rl.maxAge > 0 && !fi.ModTime().After(cutoff) {
			m[fl] = p
			outDateFiles = append(outDateFiles, fl)
		}
		if rl.rotationCount > 0 && fl.Mode()&os.ModeSymlink != os.ModeSymlink {
			m[fl] = p
			totalFiles = append(totalFiles, fl)
		}
	}
	// sort by mod time
	sort.Slice(totalFiles, func(i, j int) bool {
		return totalFiles[i].ModTime().Before(totalFiles[j].ModTime())
	})
	overRotationSizeFiles := make([]os.FileInfo, 0, len(totalFiles))
	if rl.rotationCount > 0 && uint(len(totalFiles)) > rl.rotationCount {
		overRotationSizeFiles = totalFiles[:len(totalFiles)-int(rl.rotationCount)]
	}

	var toRemove []string
	if len(overRotationSizeFiles) > len(outDateFiles) {
		for i := 0; i < len(overRotationSizeFiles); i++ {
			toRemove = append(toRemove, m[overRotationSizeFiles[i]])
		}
	} else {
		for i := 0; i < len(outDateFiles); i++ {
			toRemove = append(toRemove, m[outDateFiles[i]])
		}
	}

	for _, path := range toRemove {
		if rl.checkLogInuse(path) {
			continue
		}
		rl.removeFile(path)
	}

	if rl.compress {
		tmp := make(map[string]struct{}, len(toRemove))
		for _, item := range toRemove {
			tmp[item] = struct{}{}
		}
		currentFileName := rl.CurrentFileName()
		var tempFile string
		for i := 0; i < len(toCompress); i++ {
			// ignore compressed file and current file
			if strings.HasSuffix(toCompress[i], ".gz") || toCompress[i] == currentFileName {
				continue
			}
			if _, ok := tmp[toCompress[i]]; ok {
				continue
			}
			if rl.checkLogInuse(toCompress[i]) {
				continue
			}
			// compress file to .gz.tmp
			tempFile = toCompress[i] + ".gz.tmp"
			err = compressFile(toCompress[i], tempFile)
			if err != nil {
				rl.removeFile(tempFile)
				continue
			}
			// rename temp file to .gz
			err = os.Rename(tempFile, toCompress[i]+".gz")
			if err != nil {
				rl.removeFile(tempFile)
				continue
			}
			// remove original file
			rl.removeFile(toCompress[i])
		}
	}
	return nil
}

func (rl *RotateLogs) checkLogInuse(filename string) bool {
	// write only open file, avoid nfs file system lock file failed
	fileHandle, err := os.OpenFile(filename, os.O_WRONLY, 0)
	if err != nil {
		// if open file failed, return in use
		return true
	}
	defer fileHandle.Close()
	err = LockFile(fileHandle)
	if err != nil {
		// if lock file failed, return in use
		return true
	}
	UnlockFile(fileHandle)
	lockFile := filename + `_lock`
	// write only open file, avoid nfs file system lock file failed
	lockHandle, err := os.OpenFile(lockFile, os.O_WRONLY, 0)
	if err != nil {
		// open lock file failed, return not in use
		return false
	}
	cleanLockFile := false
	defer func() {
		if cleanLockFile {
			rl.removeFile(lockFile)
		}
	}()
	defer lockHandle.Close()
	stat, err := lockHandle.Stat()
	if err != nil {
		// get stat failed, return in use, maybe lock file is broken
		return true
	}
	if time.Now().Sub(stat.ModTime()) < time.Second*1 {
		// lock file modify time is less than 1 second, return in use, maybe lock file is not locked
		return true
	}
	err = LockFile(lockHandle)
	if err != nil {
		// lock failed, return in use
		return true
	}
	// lock success means lockfile not in use, return not in use, and remove lock file
	UnlockFile(lockHandle)
	cleanLockFile = true
	return false
}

func compressFile(fileName, gzTempFile string) error {
	// open file and lock it
	// read-write open file, avoid nfs file system lock file failed
	f, err := os.OpenFile(fileName, os.O_RDWR, 0)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("failed to open file %s", fileName))
	}
	defer f.Close()
	err = LockFile(f)
	if err != nil {
		return errors.Wrap(err, "failed to lock file")
	}
	tempFile, err := os.OpenFile(gzTempFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer tempFile.Close()
	w := gzip.NewWriter(tempFile)
	defer w.Close()
	_, err = io.Copy(w, f)
	if err != nil {
		return err
	}
	w.Flush()
	return err
}

func (rl *RotateLogs) removeFile(fileName string) error {
	// ensure the file is in the log path and not a directory
	dir := filepath.Dir(fileName)
	if dir != rl.logPath {
		return errors.Errorf("remove file error, %s is not in log path", fileName)
	}
	info, err := os.Stat(fileName)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("failed to stat file %s", fileName))
	}
	if info.IsDir() {
		return fmt.Errorf("remove file error, %s is a directory", fileName)
	}
	return os.Remove(fileName)
}

// Close satisfies the io.Closer interface. You must
// call this method if you performed any writes to
// the object.
func (rl *RotateLogs) Close() error {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()

	if rl.outFh == nil {
		return nil
	}
	close(rl.exitChan)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	select {
	case <-rl.done:
	case <-ctx.Done():
		err = errors.New("close timeout")
	}
	rl.outFh.Close()
	rl.outFh = nil
	if rl.lockFh != nil {
		UnlockFile(rl.lockFh)
		rl.lockFh.Close()
		rl.lockFh = nil
		rl.removeFile(rl.lockFilename)
	}
	return err
}
