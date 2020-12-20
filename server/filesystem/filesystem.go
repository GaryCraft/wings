package filesystem

import (
	"bufio"
	"emperror.dev/errors"
	"github.com/gabriel-vasile/mimetype"
	"github.com/karrick/godirwalk"
	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/system"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Filesystem struct {
	mu                sync.RWMutex
	lastLookupTime    *usageLookupTime
	lookupInProgress  system.AtomicBool
	diskUsed          int64
	diskCheckInterval time.Duration

	// The maximum amount of disk space (in bytes) that this Filesystem instance can use.
	diskLimit int64

	// The root data directory path for this Filesystem instance.
	root string

	isTest bool
}

// Creates a new Filesystem instance for a given server.
func New(root string, size int64) *Filesystem {
	return &Filesystem{
		root:              root,
		diskLimit:         size,
		diskCheckInterval: time.Duration(config.Get().System.DiskCheckInterval),
		lastLookupTime:    &usageLookupTime{},
	}
}

// Returns the root path for the Filesystem instance.
func (fs *Filesystem) Path() string {
	return fs.root
}

// Reads a file on the system and returns it as a byte representation in a file
// reader. This is not the most memory efficient usage since it will be reading the
// entirety of the file into memory.
func (fs *Filesystem) Readfile(p string, w io.Writer) error {
	cleaned, err := fs.SafePath(p)
	if err != nil {
		return err
	}

	if st, err := os.Stat(cleaned); err != nil {
		return err
	} else if st.IsDir() {
		return &Error{code: ErrCodeIsDirectory}
	}

	f, err := os.Open(cleaned)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = bufio.NewReader(f).WriteTo(w)

	return err
}

// Writes a file to the system. If the file does not already exist one will be created.
func (fs *Filesystem) Writefile(p string, r io.Reader) error {
	cleaned, err := fs.SafePath(p)
	if err != nil {
		return err
	}

	var currentSize int64
	// If the file does not exist on the system already go ahead and create the pathway
	// to it and an empty file. We'll then write to it later on after this completes.
	stat, err := os.Stat(cleaned)
	if err != nil && !os.IsNotExist(err) {
		return err
	} else if err == nil {
		if stat.IsDir() {
			return &Error{code: ErrCodeIsDirectory}
		}
		currentSize = stat.Size()
	}

	br := bufio.NewReader(r)
	// Check that the new size we're writing to the disk can fit. If there is currently
	// a file we'll subtract that current file size from the size of the buffer to determine
	// the amount of new data we're writing (or amount we're removing if smaller).
	if err := fs.HasSpaceFor(int64(br.Size()) - currentSize); err != nil {
		return err
	}

	// If we were unable to stat the location because it did not exist, go ahead and create
	// it now. We do this after checking the disk space so that we do not just create empty
	// directories at random.
	if err != nil {
		if err := os.MkdirAll(filepath.Dir(cleaned), 0755); err != nil {
			return err
		}
		if err := fs.Chown(filepath.Dir(cleaned)); err != nil {
			return err
		}
	}

	o := &fileOpener{}
	// This will either create the file if it does not already exist, or open and
	// truncate the existing file.
	file, err := o.open(cleaned, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer file.Close()

	buf := make([]byte, 1024*4)
	sz, err := io.CopyBuffer(file, r, buf)

	// Adjust the disk usage to account for the old size and the new size of the file.
	fs.addDisk(sz - currentSize)

	// Finally, chown the file to ensure the permissions don't end up out-of-whack
	// if we had just created it.
	return fs.Chown(cleaned)
}

// Creates a new directory (name) at a specified path (p) for the server.
func (fs *Filesystem) CreateDirectory(name string, p string) error {
	cleaned, err := fs.SafePath(path.Join(p, name))
	if err != nil {
		return err
	}

	return os.MkdirAll(cleaned, 0755)
}

// Moves (or renames) a file or directory.
func (fs *Filesystem) Rename(from string, to string) error {
	cleanedFrom, err := fs.SafePath(from)
	if err != nil {
		return err
	}

	cleanedTo, err := fs.SafePath(to)
	if err != nil {
		return err
	}

	// If the target file or directory already exists the rename function will fail, so just
	// bail out now.
	if _, err := os.Stat(cleanedTo); err == nil {
		return os.ErrExist
	}

	if cleanedTo == fs.Path() {
		return errors.New("attempting to rename into an invalid directory space")
	}

	d := strings.TrimSuffix(cleanedTo, path.Base(cleanedTo))
	// Ensure that the directory we're moving into exists correctly on the system. Only do this if
	// we're not at the root directory level.
	if d != fs.Path() {
		if mkerr := os.MkdirAll(d, 0755); mkerr != nil {
			return errors.WithMessage(mkerr, "failed to create directory structure for file rename")
		}
	}

	return os.Rename(cleanedFrom, cleanedTo)
}

// Recursively iterates over a file or directory and sets the permissions on all of the
// underlying files. Iterate over all of the files and directories. If it is a file just
// go ahead and perform the chown operation. Otherwise dig deeper into the directory until
// we've run out of directories to dig into.
func (fs *Filesystem) Chown(path string) error {
	cleaned, err := fs.SafePath(path)
	if err != nil {
		return err
	}

	if fs.isTest {
		return nil
	}

	uid := config.Get().System.User.Uid
	gid := config.Get().System.User.Gid

	// Start by just chowning the initial path that we received.
	if err := os.Chown(cleaned, uid, gid); err != nil {
		return err
	}

	// If this is not a directory we can now return from the function, there is nothing
	// left that we need to do.
	if st, _ := os.Stat(cleaned); !st.IsDir() {
		return nil
	}

	// If this was a directory, begin walking over its contents recursively and ensure that all
	// of the subfiles and directories get their permissions updated as well.
	return godirwalk.Walk(cleaned, &godirwalk.Options{
		Unsorted: true,
		Callback: func(p string, e *godirwalk.Dirent) error {
			// Do not attempt to chmod a symlink. Go's os.Chown function will affect the symlink
			// so if it points to a location outside the data directory the user would be able to
			// (un)intentionally modify that files permissions.
			if e.IsSymlink() {
				if e.IsDir() {
					return godirwalk.SkipThis
				}

				return nil
			}

			return os.Chown(p, uid, gid)
		},
	})
}

func (fs *Filesystem) Chmod(path string, mode os.FileMode) error {
	cleaned, err := fs.SafePath(path)
	if err != nil {
		return err
	}

	if fs.isTest {
		return nil
	}

	if err := os.Chmod(cleaned, mode); err != nil {
		return err
	}

	return nil
}

// Begin looping up to 50 times to try and create a unique copy file name. This will take
// an input of "file.txt" and generate "file copy.txt". If that name is already taken, it will
// then try to write "file copy 2.txt" and so on, until reaching 50 loops. At that point we
// won't waste anymore time, just use the current timestamp and make that copy.
//
// Could probably make this more efficient by checking if there are any files matching the copy
// pattern, and trying to find the highest number and then incrementing it by one rather than
// looping endlessly.
func (fs *Filesystem) findCopySuffix(dir string, name string, extension string) (string, error) {
	var i int
	var suffix = " copy"

	for i = 0; i < 51; i++ {
		if i > 0 {
			suffix = " copy " + strconv.Itoa(i)
		}

		n := name + suffix + extension
		// If we stat the file and it does not exist that means we're good to create the copy. If it
		// does exist, we'll just continue to the next loop and try again.
		if _, err := fs.Stat(path.Join(dir, n)); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return "", err
			}

			break
		}

		if i == 50 {
			suffix = "copy." + time.Now().Format(time.RFC3339)
		}
	}

	return name + suffix + extension, nil
}

// Copies a given file to the same location and appends a suffix to the file to indicate that
// it has been copied.
func (fs *Filesystem) Copy(p string) error {
	cleaned, err := fs.SafePath(p)
	if err != nil {
		return err
	}

	s, err := os.Stat(cleaned)
	if err != nil {
		return err
	} else if s.IsDir() || !s.Mode().IsRegular() {
		// If this is a directory or not a regular file, just throw a not-exist error
		// since anything calling this function should understand what that means.
		return os.ErrNotExist
	}

	// Check that copying this file wouldn't put the server over its limit.
	if err := fs.HasSpaceFor(s.Size()); err != nil {
		return err
	}

	base := filepath.Base(cleaned)
	relative := strings.TrimSuffix(strings.TrimPrefix(cleaned, fs.Path()), base)
	extension := filepath.Ext(base)
	name := strings.TrimSuffix(base, extension)

	// Ensure that ".tar" is also counted as apart of the file extension.
	// There might be a better way to handle this for other double file extensions,
	// but this is a good workaround for now.
	if strings.HasSuffix(name, ".tar") {
		extension = ".tar" + extension
		name = strings.TrimSuffix(name, ".tar")
	}

	source, err := os.Open(cleaned)
	if err != nil {
		return err
	}
	defer source.Close()

	n, err := fs.findCopySuffix(relative, name, extension)
	if err != nil {
		return err
	}

	return fs.Writefile(path.Join(relative, n), source)
}

// Deletes a file or folder from the system. Prevents the user from accidentally
// (or maliciously) removing their root server data directory.
func (fs *Filesystem) Delete(p string) error {
	wg := sync.WaitGroup{}
	// This is one of the few (only?) places in the codebase where we're explicitly not using
	// the SafePath functionality when working with user provided input. If we did, you would
	// not be able to delete a file that is a symlink pointing to a location outside of the data
	// directory.
	//
	// We also want to avoid resolving a symlink that points _within_ the data directory and thus
	// deleting the actual source file for the symlink rather than the symlink itself. For these
	// purposes just resolve the actual file path using filepath.Join() and confirm that the path
	// exists within the data directory.
	resolved := fs.unsafeFilePath(p)
	if !fs.unsafeIsInDataDirectory(resolved) {
		return NewBadPathResolution(p, resolved)
	}

	// Block any whoopsies.
	if resolved == fs.Path() {
		return errors.New("cannot delete root server directory")
	}

	if st, err := os.Lstat(resolved); err != nil {
		if !os.IsNotExist(err) {
			fs.error(err).Warn("error while attempting to stat file before deletion")
		}
	} else {
		if !st.IsDir() {
			fs.addDisk(-st.Size())
		} else {
			wg.Add(1)
			go func(wg *sync.WaitGroup, st os.FileInfo, resolved string) {
				defer wg.Done()
				if s, err := fs.DirectorySize(resolved); err == nil {
					fs.addDisk(-s)
				}
			}(&wg, st, resolved)
		}
	}

	wg.Wait()

	return os.RemoveAll(resolved)
}

type fileOpener struct {
	busy uint
}

// Attempts to open a given file up to "attempts" number of times, using a backoff. If the file
// cannot be opened because of a "text file busy" error, we will attempt until the number of attempts
// has been exhaused, at which point we will abort with an error.
func (fo *fileOpener) open(path string, flags int, perm os.FileMode) (*os.File, error) {
	for {
		f, err := os.OpenFile(path, flags, perm)

		// If there is an error because the text file is busy, go ahead and sleep for a few
		// hundred milliseconds and then try again up to three times before just returning the
		// error back to the caller.
		//
		// Based on code from: https://github.com/golang/go/issues/22220#issuecomment-336458122
		if err != nil && fo.busy < 3 && strings.Contains(err.Error(), "text file busy") {
			time.Sleep(100 * time.Millisecond << fo.busy)
			fo.busy++
			continue
		}

		return f, err
	}
}

// Lists the contents of a given directory and returns stat information about each
// file and folder within it.
func (fs *Filesystem) ListDirectory(p string) ([]*Stat, error) {
	cleaned, err := fs.SafePath(p)
	if err != nil {
		return nil, err
	}

	files, err := ioutil.ReadDir(cleaned)
	if err != nil {
		return nil, err
	}

	var wg sync.WaitGroup

	// You must initialize the output of this directory as a non-nil value otherwise
	// when it is marshaled into a JSON object you'll just get 'null' back, which will
	// break the panel badly.
	out := make([]*Stat, len(files))

	// Iterate over all of the files and directories returned and perform an async process
	// to get the mime-type for them all.
	for i, file := range files {
		wg.Add(1)

		go func(idx int, f os.FileInfo) {
			defer wg.Done()

			var m *mimetype.MIME
			var d = "inode/directory"
			if !f.IsDir() {
				cleanedp := filepath.Join(cleaned, f.Name())
				if f.Mode()&os.ModeSymlink != 0 {
					cleanedp, _ = fs.SafePath(filepath.Join(cleaned, f.Name()))
				}

				if cleanedp != "" {
					m, _ = mimetype.DetectFile(filepath.Join(cleaned, f.Name()))
				} else {
					// Just pass this for an unknown type because the file could not safely be resolved within
					// the server data path.
					d = "application/octet-stream"
				}
			}

			st := &Stat{
				Info:     f,
				Mimetype: d,
			}

			if m != nil {
				st.Mimetype = m.String()
			}

			out[idx] = st
		}(i, file)
	}

	wg.Wait()

	// Sort the output alphabetically to begin with since we've run the output
	// through an asynchronous process and the order is gonna be very random.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Info.Name() == out[j].Info.Name() || out[i].Info.Name() > out[j].Info.Name() {
			return true
		}

		return false
	})

	// Then, sort it so that directories are listed first in the output. Everything
	// will continue to be alphabetized at this point.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Info.IsDir()
	})

	return out, nil
}
