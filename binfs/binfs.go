package binfs

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shabbyrobe/bingen"
)

type FileSystem interface {
	http.FileSystem
	Preload() (FileSystem, error)
	ReadFile(path string) ([]byte, error)
}

type Config struct {
	Mode bingen.Mode
	Gzip bool
	Data interface{}
}

func (c Config) New() FileSystem {
	var out FileSystem
	if c.Mode == bingen.Base64 {
		out = &stringLazyFileSystem{config: c, data: c.Data.(map[string]string)}
	} else if c.Gzip {
		out = &byteLazyFileSystem{config: c, data: c.Data.(map[string][]byte)}
	} else {
		out = &byteFileSystem{config: c, data: c.Data.(map[string][]byte)}
	}
	return out
}

func (c Config) Preload() (FileSystem, error) {
	return c.New().Preload()
}

func (c Config) MustPreload() FileSystem {
	fs := c.New()
	pl, err := fs.Preload()
	if err != nil {
		panic(err)
	}
	return pl
}

// Override a FileSystem with a physical path - use this in
// development to mask the baked-in FileSystem with updatable
// versions stored locally.
func Override(path string, fs FileSystem) (FileSystem, error) {
	ofs, ok := fs.(*overrideFileSystem)
	if ok {
		return ofs, nil
	}
	stat, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !stat.IsDir() {
		return nil, fmt.Errorf("binfs: override path %q doesn't exist", path)
	}
	return &overrideFileSystem{path, fs}, nil
}

type overrideFileSystem struct {
	path  string
	inner FileSystem
}

func (fs *overrideFileSystem) ReadFile(name string) ([]byte, error) {
	return readFile(fs, name)
}

func (fs *overrideFileSystem) Preload() (FileSystem, error) {
	preloaded, err := fs.inner.Preload()
	if err != nil {
		return nil, err
	}
	return Override(fs.path, preloaded)
}

func (fs *overrideFileSystem) Open(name string) (http.File, error) {
	pth := filepath.FromSlash(name)
	f, err := os.Open(filepath.Join(fs.path, pth))
	if os.IsNotExist(err) {
		return fs.inner.Open(name)
	} else if err == nil {
		return f, nil
	} else {
		return nil, err
	}
}

type byteFileSystem struct {
	config Config
	data   map[string][]byte
}

func (fs *byteFileSystem) ReadFile(name string) ([]byte, error) {
	return readFile(fs, name)
}

func (fs *byteFileSystem) Preload() (FileSystem, error) {
	return fs, nil
}

func (fs *byteFileSystem) Open(name string) (http.File, error) {
	name = cleanURLPath(name)
	fileData, ok := fs.data[name]
	if !ok {
		return nil, os.ErrNotExist
	}

	return &file{
		name: name,
		size: int64(len(fileData)),
		rdr:  bytes.NewReader(fileData),
	}, nil
}

type byteLazyFileSystem struct {
	config Config
	data   map[string][]byte
	loaded map[string][]byte
	lock   sync.RWMutex
}

func (fs *byteLazyFileSystem) ReadFile(name string) ([]byte, error) {
	return readFile(fs, name)
}

func (fs *byteLazyFileSystem) Preload() (FileSystem, error) {
	fs.lock.Lock()
	defer fs.lock.Unlock()

	var out = make(map[string][]byte, len(fs.data))
	for name, file := range fs.data {
		fileData, err := readBytes(file, fs.config.Gzip)
		if err != nil {
			return nil, err
		}
		out[name] = fileData
	}

	return &byteFileSystem{config: fs.config, data: out}, nil
}

func (fs *byteLazyFileSystem) Open(name string) (http.File, error) {
	name = cleanURLPath(name)

	fs.lock.Lock()
	defer fs.lock.Unlock()

	fileData, ok := fs.loaded[name]
	if !ok {
		var err error
		decoder, err := gzip.NewReader(bytes.NewReader(fileData))
		if err != nil {
			return nil, err
		}

		fileData, err = ioutil.ReadAll(decoder)
		if err != nil {
			return nil, err
		}

		fs.loaded[name] = fileData
	}

	return &file{
		name: name,
		size: int64(len(fileData)),
		rdr:  bytes.NewReader(fileData),
	}, nil
}

type stringLazyFileSystem struct {
	config Config
	data   map[string]string
	loaded map[string][]byte
	lock   sync.RWMutex
}

func (fs *stringLazyFileSystem) ReadFile(name string) ([]byte, error) {
	return readFile(fs, name)
}

func (fs *stringLazyFileSystem) Preload() (FileSystem, error) {
	fs.lock.Lock()
	defer fs.lock.Unlock()

	var out = make(map[string][]byte, len(fs.data))
	for name, fileStr := range fs.data {
		fileData, err := readString(fileStr, fs.config.Gzip)
		if err != nil {
			return nil, err
		}
		out[name] = fileData
	}

	return &byteFileSystem{config: fs.config, data: out}, nil
}

func (fs *stringLazyFileSystem) Open(name string) (http.File, error) {
	name = cleanURLPath(name)

	fs.lock.Lock()
	defer fs.lock.Unlock()

	fileData, ok := fs.loaded[name]
	if !ok {
		fileStr, ok := fs.data[name]
		if !ok {
			return nil, os.ErrNotExist
		}

		var err error
		fileData, err = readString(fileStr, fs.config.Gzip)
		if err != nil {
			return nil, err
		}

		fs.loaded[name] = fileData
	}

	return &file{
		name: name,
		size: int64(len(fileData)),
		rdr:  bytes.NewReader(fileData),
	}, nil
}

type file struct {
	name string
	size int64
	rdr  *bytes.Reader
}

func (f *file) Name() string       { return f.name }
func (f *file) Size() int64        { return f.size }
func (f *file) Mode() os.FileMode  { return 0666 }
func (f *file) ModTime() time.Time { return time.Time{} }
func (f *file) IsDir() bool        { return false }
func (f *file) Sys() interface{}   { return nil }

func (f *file) Close() error { return nil }

func (f *file) Read(p []byte) (n int, err error) {
	if f.rdr == nil {
		return 0, io.EOF
	}
	return f.rdr.Read(p)
}

func (f *file) Seek(offset int64, whence int) (int64, error) {
	return f.rdr.Seek(offset, whence)
}

func (f *file) Readdir(count int) ([]os.FileInfo, error) {
	return nil, nil
}

func (f *file) Stat() (os.FileInfo, error) {
	return f, nil
}

func readFile(fs FileSystem, path string) ([]byte, error) {
	f, err := fs.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ioutil.ReadAll(f)
}

func readString(fileStr string, withGzip bool) ([]byte, error) {
	var decoder io.Reader = base64.NewDecoder(base64.StdEncoding, strings.NewReader(fileStr))

	var err error

	if withGzip {
		decoder, err = gzip.NewReader(decoder)
		if err != nil {
			return nil, err
		}
	}

	return ioutil.ReadAll(decoder)
}

func readBytes(fileData []byte, withGzip bool) ([]byte, error) {
	if withGzip {
		decoder, err := gzip.NewReader(bytes.NewReader(fileData))
		if err != nil {
			return nil, err
		}
		return ioutil.ReadAll(decoder)
	}
	return fileData, nil
}

func cleanURLPath(str string) string {
	return strings.TrimLeft(str, "/")
}
