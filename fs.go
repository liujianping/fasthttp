package fasthttp

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"html"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// PathRewriteFunc must return new request path based on arbitrary ctx
// info such as ctx.Path().
//
// Path rewriter is used in FS for translating the current request
// to the local filesystem path relative to FS.Root.
//
// The returned path may refer to ctx members. For example, ctx.Path().
type PathRewriteFunc func(ctx *RequestCtx) []byte

// NewPathSlashesStripper returns path rewriter, which strips slashesCount
// leading slashes from the path.
//
// Examples:
//
//   * slashesCount = 0, original path: "/foo/bar", result: "/foo/bar"
//   * slashesCount = 1, original path: "/foo/bar", result: "/bar"
//   * slashesCount = 2, original path: "/foo/bar", result: ""
//
// The returned path rewriter may be used as FS.PathRewrite .
func NewPathSlashesStripper(slashesCount int) PathRewriteFunc {
	return func(ctx *RequestCtx) []byte {
		return stripLeadingSlashes(ctx.Path(), slashesCount)
	}
}

// NewPathPrefixStripper returns path rewriter, which removes prefixSize bytes
// from the path prefix.
//
// Examples:
//
//   * prefixSize = 0, original path: "/foo/bar", result: "/foo/bar"
//   * prefixSize = 3, original path: "/foo/bar", result: "o/bar"
//   * prefixSize = 7, original path: "/foo/bar", result: "r"
//
// The returned path rewriter may be used as FS.PathRewrite .
func NewPathPrefixStripper(prefixSize int) PathRewriteFunc {
	return func(ctx *RequestCtx) []byte {
		path := ctx.Path()
		if len(path) >= prefixSize {
			path = path[prefixSize:]
		}
		return path
	}
}

// FS represents settings for request handler serving static files
// from the local filesystem.
type FS struct {
	// Path to the root directory to serve files from.
	Root string

	// List of index file names to try opening during directory access.
	//
	// For example:
	//
	//     * index.html
	//     * index.htm
	//     * my-super-index.xml
	//
	// By default the list is empty.
	IndexNames []string

	// Index pages for directories without files matching IndexNames
	// are automatically generated if set.
	//
	// Directory index generation may be quite slow for directories
	// with many files (more than 1K), so it is discouraged enabling
	// index pages' generation for such directories.
	//
	// By default index pages aren't generated.
	GenerateIndexPages bool

	// Transparently compresses responses if set to true.
	//
	// The server tries minimizing CPU usage by caching compressed files.
	// It adds FSCompressedFileSuffix suffix to the original file name and
	// tries saving the resulting compressed file under the new file name.
	// So it is advisable to give the server write access to Root
	// and to all inner folders in order to minimze CPU usage when serving
	// compressed responses.
	//
	// Transparent compression is disabled by default.
	Compress bool

	// Enables byte range requests if set to true.
	//
	// Byte range requests are disabled by default.
	AcceptByteRange bool

	// Path rewriting function.
	//
	// By default request path is not modified.
	PathRewrite PathRewriteFunc

	// The duration for files' caching.
	//
	// FSHandlerCacheDuration is used by default.
	CacheDuration time.Duration

	started bool
}

// FS adds this suffix to the original file names when trying to store
// compressed file under the new file name. See FS.Compress for details.
const FSCompressedFileSuffix = ".fasthttp.gz"

// FSHandlerCacheDuration is the duration for caching open file handles
// by FSHandler.
const FSHandlerCacheDuration = 10 * time.Second

// FSHandler returns request handler serving static files from
// the given root folder.
//
// stripSlashes indicates how many leading slashes must be stripped
// from requested path before searching requested file in the root folder.
// Examples:
//
//   * stripSlashes = 0, original path: "/foo/bar", result: "/foo/bar"
//   * stripSlashes = 1, original path: "/foo/bar", result: "/bar"
//   * stripSlashes = 2, original path: "/foo/bar", result: ""
//
// The returned request handler automatically generates index pages
// for directories without index.html.
//
// The returned handler caches requested file handles
// for FSHandlerCacheDuration.
// Make sure your program has enough 'max open files' limit aka
// 'ulimit -n' if root folder contains many files.
//
// Do not create multiple request handler instances for the same
// (root, stripSlashes) arguments - just reuse a single instance.
// Otherwise goroutine leak will occur.
func FSHandler(root string, stripSlashes int) RequestHandler {
	fs := &FS{
		Root:               root,
		IndexNames:         []string{"index.html"},
		GenerateIndexPages: true,
		AcceptByteRange:    true,
		PathRewrite:        NewPathSlashesStripper(stripSlashes),
	}
	return fs.NewRequestHandler()
}

// NewRequestHandler returns new request handler with the given FS settings.
//
// The returned handler caches requested file handles
// for FS.CacheDuration.
// Make sure your program has enough 'max open files' limit aka
// 'ulimit -n' if FS.Root folder contains many files.
//
// Do not create multiple request handlers from a single FS instance -
// just reuse a single request handler.
func (fs *FS) NewRequestHandler() RequestHandler {
	if fs.started {
		panic("BUG: NewRequestHandler() cannot be called multiple times for the same FS instance")
	}
	fs.started = true

	root := fs.Root

	// strip trailing slashes from the root path
	for len(root) > 0 && root[len(root)-1] == '/' {
		root = root[:len(root)-1]
	}

	// serve files from the current working directory if root is empty
	if len(root) == 0 {
		root = "."
	}

	cacheDuration := fs.CacheDuration
	if cacheDuration <= 0 {
		cacheDuration = FSHandlerCacheDuration
	}

	h := &fsHandler{
		root:               root,
		indexNames:         fs.IndexNames,
		pathRewrite:        fs.PathRewrite,
		generateIndexPages: fs.GenerateIndexPages,
		compress:           fs.Compress,
		acceptByteRange:    fs.AcceptByteRange,
		cacheDuration:      cacheDuration,
		cache:              make(map[string]*fsFile),
		compressedCache:    make(map[string]*fsFile),
	}

	go func() {
		var pendingFiles []*fsFile
		for {
			time.Sleep(cacheDuration / 2)
			pendingFiles = h.cleanCache(pendingFiles)
		}
	}()

	return h.handleRequest
}

type fsHandler struct {
	root               string
	indexNames         []string
	pathRewrite        PathRewriteFunc
	generateIndexPages bool
	compress           bool
	acceptByteRange    bool
	cacheDuration      time.Duration

	cache           map[string]*fsFile
	compressedCache map[string]*fsFile
	cacheLock       sync.Mutex

	smallFileReaderPool sync.Pool
}

type fsFile struct {
	h             *fsHandler
	f             *os.File
	dirIndex      []byte
	contentType   string
	contentLength int
	compressed    bool

	lastModified    time.Time
	lastModifiedStr []byte

	t            time.Time
	readersCount int

	bigFiles     []*bigFileReader
	bigFilesLock sync.Mutex
}

func (ff *fsFile) NewReader() (io.Reader, error) {
	if ff.isBig() {
		r, err := ff.bigFileReader()
		if err != nil {
			ff.decReadersCount()
		}
		return r, err
	}
	return ff.smallFileReader(), nil
}

func (ff *fsFile) smallFileReader() io.Reader {
	v := ff.h.smallFileReaderPool.Get()
	if v == nil {
		r := &fsSmallFileReader{
			ff:     ff,
			endPos: ff.contentLength,
		}
		return r
	}
	r := v.(*fsSmallFileReader)
	r.ff = ff
	r.endPos = ff.contentLength
	if r.startPos > 0 {
		panic("BUG: fsSmallFileReader with non-nil startPos found in the pool")
	}
	return r
}

// files bigger than this size are sent with sendfile
const maxSmallFileSize = 2 * 4096

func (ff *fsFile) isBig() bool {
	return ff.contentLength > maxSmallFileSize && len(ff.dirIndex) == 0
}

func (ff *fsFile) bigFileReader() (io.Reader, error) {
	if ff.f == nil {
		panic("BUG: ff.f must be non-nil in bigFileReader")
	}

	var r io.Reader

	ff.bigFilesLock.Lock()
	n := len(ff.bigFiles)
	if n > 0 {
		r = ff.bigFiles[n-1]
		ff.bigFiles = ff.bigFiles[:n-1]
	}
	ff.bigFilesLock.Unlock()

	if r != nil {
		return r, nil
	}

	f, err := os.Open(ff.f.Name())
	if err != nil {
		return nil, fmt.Errorf("cannot open already opened file: %s", err)
	}
	return &bigFileReader{
		f:  f,
		ff: ff,
		r:  f,
	}, nil
}

func (ff *fsFile) Release() {
	if ff.f != nil {
		ff.f.Close()

		if ff.isBig() {
			ff.bigFilesLock.Lock()
			for _, r := range ff.bigFiles {
				r.f.Close()
			}
			ff.bigFilesLock.Unlock()
		}
	}
}

func (ff *fsFile) decReadersCount() {
	ff.h.cacheLock.Lock()
	ff.readersCount--
	if ff.readersCount < 0 {
		panic("BUG: negative fsFile.readersCount!")
	}
	ff.h.cacheLock.Unlock()
}

// bigFileReader attempts to trigger sendfile
// for sending big files over the wire.
type bigFileReader struct {
	f  *os.File
	ff *fsFile
	r  io.Reader
	lr io.LimitedReader
}

func (r *bigFileReader) UpdateByteRange(startPos, endPos int) error {
	if _, err := r.f.Seek(int64(startPos), 0); err != nil {
		return err
	}
	r.r = &r.lr
	r.lr.R = r.f
	r.lr.N = int64(endPos - startPos + 1)
	return nil
}

func (r *bigFileReader) Read(p []byte) (int, error) {
	return r.r.Read(p)
}

func (r *bigFileReader) WriteTo(w io.Writer) (int64, error) {
	if rf, ok := w.(io.ReaderFrom); ok {
		// fast path. Senfile must be triggered
		return rf.ReadFrom(r.r)
	}

	// slow path
	return copyZeroAlloc(w, r.r)
}

func (r *bigFileReader) Close() error {
	r.r = r.f
	n, err := r.f.Seek(0, 0)
	if err == nil {
		if n != 0 {
			panic("BUG: File.Seek(0,0) returned (non-zero, nil)")
		}

		ff := r.ff
		ff.bigFilesLock.Lock()
		ff.bigFiles = append(ff.bigFiles, r)
		ff.bigFilesLock.Unlock()
	} else {
		r.f.Close()
	}
	r.ff.decReadersCount()
	return err
}

type fsSmallFileReader struct {
	ff       *fsFile
	startPos int
	endPos   int
}

func (r *fsSmallFileReader) Close() error {
	ff := r.ff
	ff.decReadersCount()
	r.ff = nil
	r.startPos = 0
	r.endPos = 0
	ff.h.smallFileReaderPool.Put(r)
	return nil
}

func (r *fsSmallFileReader) UpdateByteRange(startPos, endPos int) error {
	r.startPos = startPos
	r.endPos = endPos + 1
	return nil
}

func (r *fsSmallFileReader) Read(p []byte) (int, error) {
	tailLen := r.endPos - r.startPos
	if tailLen <= 0 {
		return 0, io.EOF
	}
	if len(p) > tailLen {
		p = p[:tailLen]
	}

	ff := r.ff
	if ff.f != nil {
		n, err := ff.f.ReadAt(p, int64(r.startPos))
		r.startPos += n
		return n, err
	}

	n := copy(p, ff.dirIndex[r.startPos:])
	r.startPos += n
	return n, nil
}

func (r *fsSmallFileReader) WriteTo(w io.Writer) (int64, error) {
	ff := r.ff

	var n int
	var err error
	if ff.f == nil {
		n, err = w.Write(ff.dirIndex[r.startPos:r.endPos])
		return int64(n), err
	}

	if rf, ok := w.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}

	curPos := r.startPos
	bufv := copyBufPool.Get()
	buf := bufv.([]byte)
	for err != nil {
		tailLen := r.endPos - curPos
		if tailLen <= 0 {
			break
		}
		if len(buf) > tailLen {
			buf = buf[:tailLen]
		}
		n, err = ff.f.ReadAt(buf, int64(curPos))
		nw, errw := w.Write(buf[:n])
		curPos += nw
		if errw == nil && nw != n {
			panic("BUG: Write(p) returned (n, nil), where n != len(p)")
		}
		if err == nil {
			err = errw
		}
	}
	copyBufPool.Put(bufv)

	if err == io.EOF {
		err = nil
	}
	return int64(curPos - r.startPos), err
}

func (h *fsHandler) cleanCache(pendingFiles []*fsFile) []*fsFile {
	var filesToRelease []*fsFile

	h.cacheLock.Lock()

	// Close files which couldn't be closed before due to non-zero
	// readers count on the previous run.
	var remainingFiles []*fsFile
	for _, ff := range pendingFiles {
		if ff.readersCount > 0 {
			remainingFiles = append(remainingFiles, ff)
		} else {
			filesToRelease = append(filesToRelease, ff)
		}
	}
	pendingFiles = remainingFiles

	pendingFiles, filesToRelease = cleanCacheNolock(h.cache, pendingFiles, filesToRelease, h.cacheDuration)
	pendingFiles, filesToRelease = cleanCacheNolock(h.compressedCache, pendingFiles, filesToRelease, h.cacheDuration)

	h.cacheLock.Unlock()

	for _, ff := range filesToRelease {
		ff.Release()
	}

	return pendingFiles
}

func cleanCacheNolock(cache map[string]*fsFile, pendingFiles, filesToRelease []*fsFile, cacheDuration time.Duration) ([]*fsFile, []*fsFile) {
	t := time.Now()
	for k, ff := range cache {
		if t.Sub(ff.t) > cacheDuration {
			if ff.readersCount > 0 {
				// There are pending readers on stale file handle,
				// so we cannot close it. Put it into pendingFiles
				// so it will be closed later.
				pendingFiles = append(pendingFiles, ff)
			} else {
				filesToRelease = append(filesToRelease, ff)
			}
			delete(cache, k)
		}
	}
	return pendingFiles, filesToRelease
}

func (h *fsHandler) handleRequest(ctx *RequestCtx) {
	var path []byte
	if h.pathRewrite != nil {
		path = h.pathRewrite(ctx)
	} else {
		path = ctx.Path()
	}
	path = stripTrailingSlashes(path)

	if n := bytes.IndexByte(path, 0); n >= 0 {
		ctx.Logger().Printf("cannot serve path with nil byte at position %d: %q", n, path)
		ctx.Error("Are you a hacker?", StatusBadRequest)
		return
	}

	mustCompress := false
	fileCache := h.cache
	byteRange := ctx.Request.Header.peek(strRange)
	if len(byteRange) == 0 && h.compress && ctx.Request.Header.HasAcceptEncodingBytes(strGzip) {
		mustCompress = true
		fileCache = h.compressedCache
	}

	h.cacheLock.Lock()
	ff, ok := fileCache[string(path)]
	if ok {
		ff.readersCount++
	}
	h.cacheLock.Unlock()

	if !ok {
		pathStr := string(path)
		filePath := h.root + pathStr
		var err error
		ff, err = h.openFSFile(filePath, mustCompress)
		if mustCompress && err == errNoCreatePermission {
			ctx.Logger().Printf("insufficient permissions for saving compressed file for %q. Serving uncompressed file. "+
				"Allow write access to the directory with this file in order to improve fasthttp performance", filePath)
			mustCompress = false
			ff, err = h.openFSFile(filePath, mustCompress)
		}
		if err == errDirIndexRequired {
			ff, err = h.openIndexFile(ctx, filePath, mustCompress)
			if err != nil {
				ctx.Logger().Printf("cannot open dir index %q: %s", filePath, err)
				ctx.Error("Directory index is forbidden", StatusForbidden)
				return
			}
		} else if err != nil {
			ctx.Logger().Printf("cannot open file %q: %s", filePath, err)
			ctx.Error("Cannot open requested path", StatusNotFound)
			return
		}

		h.cacheLock.Lock()
		ff1, ok := fileCache[pathStr]
		if !ok {
			fileCache[pathStr] = ff
			ff.readersCount++
		} else {
			ff1.readersCount++
		}
		h.cacheLock.Unlock()

		if ok {
			// The file has been already opened by another
			// goroutine, so close the current file and use
			// the file opened by another goroutine instead.
			ff.Release()
			ff = ff1
		}
	}

	if !ctx.IfModifiedSince(ff.lastModified) {
		ff.decReadersCount()
		ctx.NotModified()
		return
	}

	r, err := ff.NewReader()
	if err != nil {
		ctx.Logger().Printf("cannot obtain file reader for path=%q: %s", path, err)
		ctx.Error("Internal Server Error", StatusInternalServerError)
		return
	}

	hdr := &ctx.Response.Header
	if ff.compressed {
		hdr.SetCanonical(strContentEncoding, strGzip)
	}

	statusCode := StatusOK
	contentLength := ff.contentLength
	if h.acceptByteRange {
		hdr.SetCanonical(strAcceptRanges, strBytes)
		if len(byteRange) > 0 {
			startPos, endPos, err := ParseByteRange(byteRange, contentLength)
			if err != nil {
				r.(io.Closer).Close()
				ctx.Logger().Printf("cannot parse byte range %q for path=%q: %s", byteRange, path, err)
				ctx.Error("Range Not Satisfiable", StatusRequestedRangeNotSatisfiable)
				return
			}

			if err = r.(byteRangeUpdater).UpdateByteRange(startPos, endPos); err != nil {
				r.(io.Closer).Close()
				ctx.Logger().Printf("cannot seek byte range %q for path=%q: %s", byteRange, path, err)
				ctx.Error("Internal Server Error", StatusInternalServerError)
				return
			}

			hdr.SetContentRange(startPos, endPos, contentLength)
			contentLength = endPos - startPos + 1
			statusCode = StatusPartialContent
		}
	}

	hdr.SetCanonical(strLastModified, ff.lastModifiedStr)
	ctx.SetBodyStream(r, contentLength)
	ctx.SetContentType(ff.contentType)
	ctx.SetStatusCode(statusCode)
}

type byteRangeUpdater interface {
	UpdateByteRange(startPos, endPos int) error
}

var (
	errUnsupportedRangeUnits = errors.New("unsupported range units")
	errInvalidByteRange      = errors.New("invalid byte range")
)

// ParseByteRange parses 'Range: bytes=...' header value.
func ParseByteRange(byteRange []byte, contentLength int) (startPos, endPos int, err error) {
	b := byteRange
	if !bytes.HasPrefix(b, strBytes) {
		return 0, 0, errUnsupportedRangeUnits
	}

	b = b[len(strBytes):]
	if len(b) == 0 || b[0] != '=' {
		return 0, 0, errInvalidByteRange
	}
	b = b[1:]

	n := bytes.IndexByte(b, '-')
	if n < 0 {
		return 0, 0, errInvalidByteRange
	}

	if n == 0 {
		v, err := ParseUint(b[n+1:])
		if err != nil {
			return 0, 0, err
		}
		startPos := contentLength - v
		if startPos < 0 {
			return 0, 0, errInvalidByteRange
		}
		return startPos, contentLength - 1, nil
	}

	if startPos, err = ParseUint(b[:n]); err != nil {
		return 0, 0, err
	}
	if startPos >= contentLength {
		return 0, 0, errInvalidByteRange
	}

	b = b[n+1:]
	if len(b) == 0 {
		return startPos, contentLength - 1, nil
	}

	if endPos, err = ParseUint(b); err != nil {
		return 0, 0, err
	}
	if endPos >= contentLength {
		return 0, 0, errInvalidByteRange
	}
	if endPos < startPos {
		return 0, 0, errInvalidByteRange
	}
	return startPos, endPos, nil
}

func (h *fsHandler) openIndexFile(ctx *RequestCtx, dirPath string, mustCompress bool) (*fsFile, error) {
	for _, indexName := range h.indexNames {
		indexFilePath := dirPath + "/" + indexName
		ff, err := h.openFSFile(indexFilePath, mustCompress)
		if err == nil {
			return ff, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("cannot open file %q: %s", indexFilePath, err)
		}
	}

	if !h.generateIndexPages {
		return nil, fmt.Errorf("cannot access directory without index page. Directory %q", dirPath)
	}

	return h.createDirIndex(ctx.URI(), dirPath, mustCompress)
}

var (
	errDirIndexRequired   = errors.New("directory index required")
	errNoCreatePermission = errors.New("no 'create file' permissions")
)

func (h *fsHandler) createDirIndex(base *URI, dirPath string, mustCompress bool) (*fsFile, error) {
	var buf bytes.Buffer
	w := &buf

	basePathEscaped := html.EscapeString(string(base.Path()))
	fmt.Fprintf(w, "<html><head><title>%s</title><style>.dir { font-weight: bold }</style></head><body>", basePathEscaped)
	fmt.Fprintf(w, "<h1>%s</h1>", basePathEscaped)
	fmt.Fprintf(w, "<ul>")

	if len(basePathEscaped) > 1 {
		var parentURI URI
		base.CopyTo(&parentURI)
		parentURI.Update(string(base.Path()) + "/..")
		parentPathEscaped := html.EscapeString(string(parentURI.Path()))
		fmt.Fprintf(w, `<li><a href="%s" class="dir">..</a></li>`, parentPathEscaped)
	}

	f, err := os.Open(dirPath)
	if err != nil {
		return nil, err
	}

	fileinfos, err := f.Readdir(0)
	f.Close()
	if err != nil {
		return nil, err
	}

	fm := make(map[string]os.FileInfo, len(fileinfos))
	var filenames []string
	for _, fi := range fileinfos {
		name := fi.Name()
		if strings.HasSuffix(name, FSCompressedFileSuffix) {
			// Do not show compressed files on index page.
			continue
		}
		fm[name] = fi
		filenames = append(filenames, name)
	}

	var u URI
	base.CopyTo(&u)
	u.Update(string(u.Path()) + "/")

	sort.Sort(sort.StringSlice(filenames))
	for _, name := range filenames {
		u.Update(name)
		pathEscaped := html.EscapeString(string(u.Path()))
		fi := fm[name]
		auxStr := "dir"
		className := "dir"
		if !fi.IsDir() {
			auxStr = fmt.Sprintf("file, %d bytes", fi.Size())
			className = "file"
		}
		fmt.Fprintf(w, `<li><a href="%s" class="%s">%s</a>, %s, last modified %s</li>`,
			pathEscaped, className, html.EscapeString(name), auxStr, fsModTime(fi.ModTime()))
	}

	fmt.Fprintf(w, "</ul></body></html>")

	if mustCompress {
		var zbuf bytes.Buffer
		zw := acquireGzipWriter(&zbuf, CompressDefaultCompression)
		_, err = io.Copy(zw, &buf)
		releaseGzipWriter(zw)
		if err != nil {
			return nil, fmt.Errorf("error when compressing automatically generated index for directory %q: %s", dirPath, err)
		}
		w = &zbuf
	}

	dirIndex := w.Bytes()
	lastModified := time.Now()
	ff := &fsFile{
		h:               h,
		dirIndex:        dirIndex,
		contentType:     "text/html; charset=utf-8",
		contentLength:   len(dirIndex),
		compressed:      mustCompress,
		lastModified:    lastModified,
		lastModifiedStr: AppendHTTPDate(nil, lastModified),

		t: lastModified,
	}
	return ff, nil
}

const (
	fsMinCompressRatio        = 0.8
	fsMaxCompressibleFileSize = 8 * 1024 * 1024
)

func (h *fsHandler) compressAndOpenFSFile(filePath string) (*fsFile, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}

	fileInfo, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("cannot obtain info for file %q: %s", filePath, err)
	}

	if fileInfo.IsDir() {
		f.Close()
		return nil, errDirIndexRequired
	}

	if strings.HasSuffix(filePath, FSCompressedFileSuffix) ||
		fileInfo.Size() > fsMaxCompressibleFileSize ||
		!isFileCompressible(f, fsMinCompressRatio) {
		return h.newFSFile(f, fileInfo, false)
	}

	compressedFilePath := filePath + FSCompressedFileSuffix
	absPath, err := filepath.Abs(compressedFilePath)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("cannot determine absolute path for %q: %s", compressedFilePath, err)
	}

	flock := getFileLock(absPath)
	flock.Lock()
	ff, err := h.compressFileNolock(f, fileInfo, filePath, compressedFilePath)
	flock.Unlock()

	return ff, err
}

func (h *fsHandler) compressFileNolock(f *os.File, fileInfo os.FileInfo, filePath, compressedFilePath string) (*fsFile, error) {
	// Attempt to open compressed file created by another concurrent
	// goroutine.
	// It is safe opening such a file, since the file creation
	// is guarded by file mutex - see getFileLock call.
	if _, err := os.Stat(compressedFilePath); err == nil {
		f.Close()
		return h.newCompressedFSFile(compressedFilePath)
	}

	// Create temporary file, so concurrent goroutines don't use
	// it until it is created.
	tmpFilePath := compressedFilePath + ".tmp"
	zf, err := os.Create(tmpFilePath)
	if err != nil {
		f.Close()
		if !os.IsPermission(err) {
			return nil, fmt.Errorf("cannot create temporary file %q: %s", tmpFilePath, err)
		}
		return nil, errNoCreatePermission
	}

	zw := acquireGzipWriter(zf, CompressDefaultCompression)
	_, err = io.Copy(zw, f)
	if err1 := zw.Flush(); err == nil {
		err = err1
	}
	releaseGzipWriter(zw)
	zf.Close()
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("error when compressing file %q to %q: %s", filePath, tmpFilePath, err)
	}
	if err = os.Chtimes(tmpFilePath, time.Now(), fileInfo.ModTime()); err != nil {
		return nil, fmt.Errorf("cannot change modification time to %s for tmp file %q: %s",
			fileInfo.ModTime(), tmpFilePath, err)
	}
	if err = os.Rename(tmpFilePath, compressedFilePath); err != nil {
		return nil, fmt.Errorf("cannot move compressed file from %q to %q: %s", tmpFilePath, compressedFilePath, err)
	}
	return h.newCompressedFSFile(compressedFilePath)
}

func (h *fsHandler) newCompressedFSFile(filePath string) (*fsFile, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("cannot open compressed file %q: %s", filePath, err)
	}
	fileInfo, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("cannot obtain info for compressed file %q: %s", filePath, err)
	}
	return h.newFSFile(f, fileInfo, true)
}

func (h *fsHandler) openFSFile(filePath string, mustCompress bool) (*fsFile, error) {
	filePathOriginal := filePath
	if mustCompress {
		filePath += FSCompressedFileSuffix
	}

	f, err := os.Open(filePath)
	if err != nil {
		if mustCompress && os.IsNotExist(err) {
			return h.compressAndOpenFSFile(filePathOriginal)
		}
		return nil, err
	}

	fileInfo, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("cannot obtain info for file %q: %s", filePath, err)
	}

	if fileInfo.IsDir() {
		f.Close()
		if mustCompress {
			return nil, fmt.Errorf("directory with unexpected suffix found: %q. Suffix: %q", filePath, FSCompressedFileSuffix)
		}
		return nil, errDirIndexRequired
	}

	if mustCompress {
		fileInfoOriginal, err := os.Stat(filePathOriginal)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("cannot obtain info for original file %q: %s", filePathOriginal, err)
		}

		if fileInfoOriginal.ModTime() != fileInfo.ModTime() {
			// The compressed file became stale. Re-create it.
			f.Close()
			os.Remove(filePath)
			return h.compressAndOpenFSFile(filePathOriginal)
		}
	}

	return h.newFSFile(f, fileInfo, mustCompress)
}

func (h *fsHandler) newFSFile(f *os.File, fileInfo os.FileInfo, compressed bool) (*fsFile, error) {
	n := fileInfo.Size()
	contentLength := int(n)
	if n != int64(contentLength) {
		f.Close()
		return nil, fmt.Errorf("too big file: %d bytes", n)
	}

	// detect content-type
	ext := fileExtension(fileInfo.Name(), compressed)
	contentType := mime.TypeByExtension(ext)
	if len(contentType) == 0 {
		data, err := readFileHeader(f, compressed)
		if err != nil {
			return nil, fmt.Errorf("cannot read header of the file %q: %s", f.Name(), err)
		}
		contentType = http.DetectContentType(data)
	}

	lastModified := fileInfo.ModTime()
	ff := &fsFile{
		h:               h,
		f:               f,
		contentType:     contentType,
		contentLength:   contentLength,
		compressed:      compressed,
		lastModified:    lastModified,
		lastModifiedStr: AppendHTTPDate(nil, lastModified),

		t: time.Now(),
	}
	return ff, nil
}

func readFileHeader(f *os.File, compressed bool) ([]byte, error) {
	r := io.Reader(f)
	var zr *gzip.Reader
	if compressed {
		var err error
		if zr, err = acquireGzipReader(f); err != nil {
			return nil, err
		}
		r = zr
	}

	lr := &io.LimitedReader{
		R: r,
		N: 512,
	}
	data, err := ioutil.ReadAll(lr)
	f.Seek(0, 0)

	if zr != nil {
		releaseGzipReader(zr)
	}

	return data, err
}

func stripLeadingSlashes(path []byte, stripSlashes int) []byte {
	for stripSlashes > 0 && len(path) > 0 {
		if path[0] != '/' {
			panic("BUG: path must start with slash")
		}
		n := bytes.IndexByte(path[1:], '/')
		if n < 0 {
			path = path[:0]
			break
		}
		path = path[n+1:]
		stripSlashes--
	}
	return path
}

func stripTrailingSlashes(path []byte) []byte {
	for len(path) > 0 && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}
	return path
}

func fileExtension(path string, compressed bool) string {
	if compressed && strings.HasSuffix(path, FSCompressedFileSuffix) {
		path = path[:len(path)-len(FSCompressedFileSuffix)]
	}
	n := strings.LastIndexByte(path, '.')
	if n < 0 {
		return ""
	}
	return path[n:]
}

func fsLastModified(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return zeroTime, err
	}
	fileInfo, err := f.Stat()
	f.Close()
	if err != nil {
		return zeroTime, err
	}
	return fsModTime(fileInfo.ModTime()), nil
}

func fsModTime(t time.Time) time.Time {
	return t.In(time.UTC).Truncate(time.Second)
}

var (
	filesLockMap     = make(map[string]*sync.Mutex)
	filesLockMapLock sync.Mutex
)

func getFileLock(absPath string) *sync.Mutex {
	filesLockMapLock.Lock()
	flock := filesLockMap[absPath]
	if flock == nil {
		flock = &sync.Mutex{}
		filesLockMap[absPath] = flock
	}
	filesLockMapLock.Unlock()
	return flock
}
