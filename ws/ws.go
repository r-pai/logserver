package ws

import (
	"bufio"
	"log"
	"net/http"
	"path/filepath"
	"sync"

	"regexp"

	"fmt"

	"github.com/Stratoscale/logserver/config"
	"github.com/Stratoscale/logserver/parser"
	"github.com/gorilla/websocket"
	"github.com/kr/fs"
)

func New(c config.Config) http.Handler {
	return &handler{
		Config: c,
	}
}

type handler struct {
	config.Config
}

type Metadata struct {
	ID     int    `json:"id"`
	Action string `json:"action"`
}

type Request struct {
	Metadata `json:"meta"`
	Path     pathArr `json:"path"`
	Regexp   string  `json:"regexp"`
}

type pathArr []string

type fsElement struct {
	Key       string         `json:"key"`
	Path      pathArr        `json:"path"`
	IsDir     bool           `json:"is_dir"`
	Instances []fileInstance `json:"instances"`
}

type fileInstance struct {
	Size int64  `json:"size"`
	FS   string `json:"fs"`
}

type ResponseFileTree struct {
	Metadata `json:"meta"`
	Tree     []fsElement `json:"tree"`
}

type ResponseContent struct {
	Metadata `json:"meta"`
	Lines    []parser.LogLine `json:"lines"`
}

type ResponseError struct {
	Metadata `json:"meta"`
	Error    string
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("Got ws Request from: %s", r.RemoteAddr)
	u := new(websocket.Upgrader)
	conn, err := u.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	ch := make(chan interface{})
	go reader(conn, ch)

	for {
		var r Request
		err = conn.ReadJSON(&r)
		if err != nil {
			log.Printf("read: %s", err)
			return
		}
		go h.serve(ch, r)
	}
}

func reader(conn *websocket.Conn, ch <-chan interface{}) {
	for req := range ch {
		err := conn.WriteJSON(req)
		if err != nil {
			log.Printf("write: %s", err)
		}
	}
}

func (h *handler) serve(ch chan<- interface{}, r Request) {
	defer close(ch)
	path := filepath.Join(r.Path...)
	if path == "" {
		path = "/"
	}
	switch r.Action {
	case "get-file-tree":
		var (
			fsElements []fsElement
			m          = make(map[string]*fsElement)
		)

		for _, node := range h.Nodes {
			walker := fs.WalkFS(path, node.FS)
			for walker.Step() {
				if err := walker.Err(); err != nil {
					log.Printf("walk: %s", err)
					continue
				}

				key := walker.Path()
				element := m[key]
				if element == nil {
					fsElements = append(fsElements, fsElement{
						Key:   key,
						Path:  filepath.SplitList(key),
						IsDir: walker.Stat().IsDir(),
					})
					m[key] = &fsElements[len(fsElements)-1]
				}
				m[key].Instances = append(m[key].Instances, fileInstance{
					Size: walker.Stat().Size(),
					FS:   node.Name,
				})
			}
		}
		// reply
		ch <- &ResponseFileTree{
			Metadata: r.Metadata,
			Tree:     fsElements,
		}

	case "get-content":
		wg := sync.WaitGroup{}
		wg.Add(len(h.Nodes))
		for _, node := range h.Nodes {
			go func(node config.Src) {
				h.search(ch, r, node, path, nil)
				wg.Done()
			}(node)
		}
		wg.Wait()

	case "search":
		re, err := regexp.Compile(r.Regexp)
		if err != nil {
			ch <- &ResponseError{
				Metadata: r.Metadata,
				Error:    fmt.Sprintf("Bad regexp %s: %s", r.Regexp, err),
			}
		}
		wg := sync.WaitGroup{}
		wg.Add(len(h.Nodes))
		for _, node := range h.Nodes {
			go func(node config.Src) {
				h.search(ch, r, node, path, re)
				wg.Done()
			}(node)
		}
		wg.Wait()
	}
}

func (h *handler) search(ch chan<- interface{}, req Request, node config.Src, path string, re *regexp.Regexp) {
	var walker = fs.WalkFS(path, node.FS)
	for walker.Step() {
		if err := walker.Err(); err != nil {
			log.Printf("walk: %s", err)
			continue
		}
		filePath := walker.Path()

		h.read(ch, req, node, filePath, re)
	}
}

func (h *handler) read(ch chan<- interface{}, req Request, node config.Src, path string, re *regexp.Regexp) {
	r, err := node.FS.Open(path)
	if err != nil {
		log.Printf("Open %s: %s", path, err)
		return
	}
	defer r.Close()

	var (
		pars       = parser.GetParser(filepath.Ext(path))
		scanner    = bufio.NewScanner(r)
		logLines   []parser.LogLine
		lineNumber = 1
		fileOffset = 0
	)
	for scanner.Scan() {
		if err := scanner.Err(); err != nil {
			log.Println("scan:", err)
		}
		if re != nil && !re.Match(scanner.Bytes()) {
			continue
		}

		logLine, err := pars(scanner.Bytes())
		if err != nil {
			log.Println("Failed to pars line:", err)
		}
		logLine.FileName = path
		logLine.Offset = fileOffset
		logLine.FS = node.Name
		logLine.LineNumber = lineNumber

		logLines = append(logLines, *logLine)

		lineNumber += 1
		fileOffset += len(scanner.Bytes())

		// if we read lines more than the defined batch size, send them to the client and continue
		if len(logLines) > h.Config.ContentBatchSize {
			ch <- &ResponseContent{Metadata: req.Metadata, Lines: logLines}
			logLines = nil
		}
	}
	if err := scanner.Err(); err != nil {
		log.Println("Reading standard input:", err)
		return
	}

	ch <- &ResponseContent{Metadata: req.Metadata, Lines: logLines}
}
