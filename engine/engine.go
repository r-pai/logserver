package engine

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/Stratoscale/logserver/debug"
	"github.com/Stratoscale/logserver/parse"
	"github.com/Stratoscale/logserver/source"
	"github.com/bluele/gcache"
	"github.com/gorilla/websocket"
	"github.com/kr/fs"
)

var log = logrus.WithField("pkg", "ws")

const (
	defaultContentBatchSize = 2000
	defaultContentBatchTime = time.Second * 2
	defaultSearchMaxSize    = 5000
)

// Config are global configuration parameter for logserver
type Config struct {
	ContentBatchSize  int           `json:"content_batch_size"`
	ContentBatchTime  time.Duration `json:"content_batch_time"`
	SearchMaxSize     int           `json:"search_max_size"`
	CacheExpiration   time.Duration `json:"cache_expiration"`
	ExcludeExtensions []string      `json:"exclude_extensions"`
	ExcludeDirs       []string      `json:"exclude_dirs"`
}

// New returns a new websocket handler
func New(c Config, source source.Sources, parser parse.Parse, cache gcache.Cache) http.Handler {
	if c.ContentBatchSize == 0 {
		c.ContentBatchSize = defaultContentBatchSize
	}
	if c.ContentBatchTime == 0 {
		c.ContentBatchTime = defaultContentBatchTime
	}
	if c.SearchMaxSize == 0 {
		c.SearchMaxSize = defaultSearchMaxSize
	}
	h := &handler{
		Config:            c,
		source:            source,
		parse:             parser,
		cache:             cache,
		excludeDirs:       list2Map(c.ExcludeDirs),
		excludeExtensions: list2Map(c.ExcludeExtensions),
	}
	return h
}

type handler struct {
	Config
	source            source.Sources
	parse             parse.Parse
	cache             gcache.Cache
	excludeDirs       map[string]bool
	excludeExtensions map[string]bool
}

// Path describes a file path
// Each directory or file is an item in the slice
type Path []string

// Meta is request/response metadata
type Meta struct {
	ID     int    `json:"id"`
	Action string `json:"action"`
	FS     string `json:"fs,omitempty"`
	Path   Path   `json:"path,omitempty"`
}

// Request from client
type Request struct {
	Meta         `json:"meta"`
	Path         Path      `json:"path"`
	Regexp       string    `json:"regexp"`
	FilterSource []string  `json:"filter_fs"`
	FilterTime   TimeRange `json:"filter_time"`

	filterSourceMap map[string]bool
}

func (r *Request) Init() {
	r.filterSourceMap = sourceSet(r.FilterSource)
}

type TimeRange struct {
	Start *time.Time `json:"start"`
	End   *time.Time `json:"end"`
}

// Response from the server
type Response struct {
	Meta     `json:"meta"`
	Lines    []parse.Log `json:"lines,omitempty"`
	Files    []*File     `json:"tree,omitempty"`
	Error    string      `json:"error,omitempty"`
	Finished bool        `json:"finished,omitempty"`
}

func (r Response) FilterSources(sources map[string]bool) *Response {
	if len(sources) == 0 {
		return &r
	}
	files := make([]*File, 0, len(r.Files))
	for _, file := range r.Files {
		// add file to files only if it exists
		if f := file.FilterSources(sources); f != nil {
			files = append(files, f)
		}
	}
	r.Files = files
	return &r
}

// File describes a file in multiple file systems
type File struct {
	Key   string `json:"key"`
	Path  Path   `json:"path"`
	IsDir bool   `json:"is_dir"`
	// Instances are all the instances of the same file in different file systems
	Instances []FileInstance `json:"instances"`
}

func (f File) FilterSources(sources map[string]bool) *File {
	instances := make([]FileInstance, 0, len(sources))
	for _, instance := range f.Instances {
		if sources[instance.FS] {
			instances = append(instances, instance)
		}
	}
	// if file has no instances after filter - there is no actual file
	if len(instances) == 0 {
		return nil
	}
	f.Instances = instances
	return &f
}

// FileInstance describe a file on a filesystem
type FileInstance struct {
	Size int64  `json:"size"`
	FS   string `json:"fs"`
}

func list2Map(list []string) map[string]bool {
	ret := make(map[string]bool, len(list))
	for _, s := range list {
		ret[s] = true
	}
	return ret
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Infof("New WS Client from: %s", r.RemoteAddr)
	defer log.Info("Disconnected WS Client from: %s", r.RemoteAddr)
	u := &websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	conn, err := u.Upgrade(w, r, nil)
	if err != nil {
		log.WithError(err).Errorf("Failed upgrade from %s", r.RemoteAddr)
		return
	}

	send := make(chan *Response)
	go reader(conn, send)

	var (
		ctx    context.Context
		cancel context.CancelFunc
		serves sync.WaitGroup
	)

	defer func() {
		// cancel last serving if exists
		if cancel != nil {
			cancel()
		}
		// wait for all servings to finish
		serves.Wait()
		// close send channel to stop reader
		close(send)
	}()

	for {
		var req Request
		err = conn.ReadJSON(&req)
		if err != nil {
			log.WithError(err).Errorf("Failed read")
			return
		}
		req.Init()

		// cancel the last serving up on a new request
		if cancel != nil {
			cancel()
		}
		ctx, cancel = context.WithCancel(r.Context())
		serves.Add(1)
		go func() {
			h.serve(ctx, req, send)
			serves.Done()
		}()
	}

}

func reader(conn *websocket.Conn, ch <-chan *Response) {
	for req := range ch {
		err := conn.WriteJSON(req)
		if err != nil {
			log.WithError(err).Errorf("Failed write")
		}
	}
}

func (h *handler) serve(ctx context.Context, req Request, send chan<- *Response) {
	defer debug.Time(log, "Request %+v", req.Meta)()

	switch req.Action {
	case "get-file-tree":
		h.serveTree(ctx, req, send)

	case "get-content":
		h.serveContent(ctx, req, send)

	case "search":
		h.search(ctx, req, send)
	}

	if err := ctx.Err(); err != nil {
		log.Debugf("Request %d cancelled", req.ID)
	}
	send <- &Response{Meta: req.Meta, Finished: true}
}

type treeCacheKey string

func (h *handler) serveTree(ctx context.Context, req Request, send chan<- *Response) {
	var (
		cacheKey = treeCacheKey(filepath.Join(req.Path...))
		resp     *Response
	)
	if val, err := h.cache.Get(cacheKey); err == nil {
		resp = val.(*Response)
	} else {
		// if not cached, load from all sources
		var (
			c       = newCombiner()
			wg      sync.WaitGroup
			sources = h.source
		)
		wg.Add(len(sources))
		for _, src := range sources {
			go func(src source.Source) {
				defer wg.Done()
				h.srcTree(ctx, req, src, c)
			}(src)
		}
		wg.Wait()
		log.Debugf("Serve tree for %v with %d files", req.Path, len(c.files))
		resp = &Response{Meta: req.Meta, Files: c.files}
		if err := h.cache.Set(cacheKey, resp); err != nil {
			log.WithError(err).Warnf("Set cache")
		}
	}

	resp = resp.FilterSources(req.filterSourceMap)
	resp.ID = req.ID
	send <- resp
}

func (h *handler) recurseTree(ctx context.Context, path string, src source.Source, f func(*fs.Walker)) {
	walker := fs.WalkFS(path, src.FS)
	for walker.Step() {
		if err := ctx.Err(); err != nil {
			return
		}

		if err := walker.Err(); err != nil {
			log.WithError(err).Errorf("Failed walk %s:%s", src.Name, path)
			continue
		}

		if walker.Stat().IsDir() {
			if _, ok := h.excludeDirs[filepath.Base(walker.Path())]; ok {
				walker.SkipDir()
				continue
			}
		} else {
			if _, ok := h.excludeExtensions[filepath.Ext(walker.Path())]; ok {
				continue
			}
		}

		f(walker)
	}
}

// srcTree returns a file tree from a single source
func (h *handler) srcTree(ctx context.Context, req Request, src source.Source, c *combiner) {
	const sep = string(os.PathSeparator)
	path := src.FS.Join(req.Path...)

	h.recurseTree(ctx, path, src, func(walker *fs.Walker) {
		key := strings.Trim(walker.Path(), sep)
		if key == "" {
			return
		}

		c.add(
			File{
				Key:   key,
				Path:  strings.Split(key, sep),
				IsDir: walker.Stat().IsDir(),
			},
			FileInstance{
				Size: walker.Stat().Size(),
				FS:   src.Name,
			},
		)
	})
}

type combiner struct {
	files []*File
	index map[string]*File
	lock  sync.Mutex
}

func newCombiner() *combiner {
	return &combiner{index: make(map[string]*File)}
}

func (c *combiner) add(f File, instance FileInstance) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.index[f.Key] == nil {
		c.files = append(c.files, &f)
		c.index[f.Key] = c.files[len(c.files)-1]
	}
	c.index[f.Key].Instances = append(c.index[f.Key].Instances, instance)
}

func (h *handler) serveContent(ctx context.Context, req Request, send chan<- *Response) {
	wg := sync.WaitGroup{}
	sources := filterSources(h.source, req.filterSourceMap)
	wg.Add(len(sources))
	for _, src := range sources {
		go func(src source.Source) {
			defer wg.Done()
			path := src.FS.Join(req.Path...)
			h.read(ctx, send, req, src, path, nil)
		}(src)
	}
	wg.Wait()
}

func (h *handler) search(ctx context.Context, req Request, send chan<- *Response) {
	re, err := regexp.Compile(req.Regexp)
	if err != nil {
		send <- &Response{
			Meta:  req.Meta,
			Error: fmt.Sprintf("Bad regexp %s: %s", req.Regexp, err),
		}
		return
	}
	nodes := filterSources(h.source, req.filterSourceMap)
	wg := sync.WaitGroup{}
	wg.Add(len(nodes))
	for _, node := range nodes {
		go func(node source.Source) {
			defer wg.Done()
			path := node.FS.Join(req.Path...)
			h.searchNode(ctx, send, req, node, path, re)
		}(node)
	}
	wg.Wait()
}

func (h *handler) searchNode(ctx context.Context, send chan<- *Response, req Request, node source.Source, path string, re *regexp.Regexp) {
	h.recurseTree(ctx, path, node, func(walker *fs.Walker) {
		filePath := walker.Path()
		h.read(ctx, send, req, node, filePath, re)
	})
}

func (h *handler) read(ctx context.Context, send chan<- *Response, req Request, node source.Source, path string, re *regexp.Regexp) {
	log := log.WithField("path", fmt.Sprintf("%s:%s", node.Name, path))
	stat, err := node.FS.Lstat(path)
	if err != nil {
		// the file might not exists in all filesystem, so just return without an error
		return
	}
	if stat.IsDir() {
		return
	}

	r, err := node.FS.Open(path)
	if err != nil {
		log.WithError(err).Error("Failed open")
		return
	}
	defer r.Close()

	var (
		scanner      = bufio.NewScanner(r)
		logLines     []parse.Log
		lastRespTime = time.Now()
		lineNumber   = 1
		fileOffset   = 0
		respMeta     = Meta{
			ID:     req.Meta.ID,
			Action: req.Meta.Action,
			FS:     node.Name,
			Path:   strings.Split(path, "/"),
		}
		sentAny      = false
		parserMemory = new(parse.Memory)
	)

	// set initial buffer size to 64kb and allow it to increase up to 1mb
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	if respMeta.Path[0] == "" {
		respMeta.Path = respMeta.Path[1:]
	}

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return
		}
		line := h.parse.Parse(path, scanner.Bytes(), parserMemory)

		// if a search was defined, check for match and if no match was found continue
		// without sending the line
		if re != nil && !re.MatchString(line.Msg) {
			lineNumber += 1
			fileOffset += len(scanner.Bytes())
			continue
		}

		line.FileName = path
		line.Offset = fileOffset
		line.FS = node.Name
		line.Line = lineNumber

		if filterOutTime(line, req.FilterTime) {
			continue
		}

		logLines = append(logLines, *line)
		lineNumber += 1
		fileOffset += len(scanner.Bytes())

		// if we read lines more than the defined batch size or batch time,
		// send them to the client and continue
		if len(logLines) > h.ContentBatchSize || time.Now().Sub(lastRespTime) > h.ContentBatchTime {
			sentAny = true
			send <- &Response{Meta: respMeta, Lines: logLines}
			logLines = nil
			lastRespTime = time.Now()
		}
		// max search lines exceeded
		if re != nil && len(logLines) > h.SearchMaxSize {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		log.WithError(err).Errorf("Failed scan")
		return
	}
	if len(logLines) == 0 && (sentAny || re != nil) {
		return
	}
	send <- &Response{Meta: respMeta, Lines: logLines}

}

func sourceSet(sourceList []string) map[string]bool {
	sources := make(map[string]bool, len(sourceList))
	for _, node := range sourceList {
		sources[node] = true
	}
	return sources
}

func filterSources(sources []source.Source, filterSources map[string]bool) []source.Source {
	if len(filterSources) == 0 {
		return sources
	}
	ret := make([]source.Source, 0, len(filterSources))
	for _, src := range sources {
		if filterSources[src.Name] {
			ret = append(ret, src)
		}
	}
	return ret
}

func filterOutTime(line *parse.Log, timeRange TimeRange) bool {
	if start := timeRange.Start; start != nil {
		return line.Time == nil || start.After(*line.Time)
	}
	if end := timeRange.End; end != nil {
		return line.Time == nil || end.Before(*line.Time)
	}
	return false
}
