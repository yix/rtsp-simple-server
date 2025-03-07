package core

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	gopath "path"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/aler9/rtsp-simple-server/internal/conf"
	"github.com/aler9/rtsp-simple-server/internal/logger"
)

type hlsServerAPIMuxersListItem struct {
	LastRequest string `json:"lastRequest"`
}

type hlsServerAPIMuxersListData struct {
	Items map[string]hlsServerAPIMuxersListItem `json:"items"`
}

type hlsServerAPIMuxersListRes struct {
	data   *hlsServerAPIMuxersListData
	muxers map[string]*hlsMuxer
	err    error
}

type hlsServerAPIMuxersListReq struct {
	res chan hlsServerAPIMuxersListRes
}

type hlsServerAPIMuxersListSubReq struct {
	data *hlsServerAPIMuxersListData
	res  chan struct{}
}

type hlsServerParent interface {
	Log(logger.Level, string, ...interface{})
}

type hlsServer struct {
	externalAuthenticationURL string
	hlsAlwaysRemux            bool
	hlsSegmentCount           int
	hlsSegmentDuration        conf.StringDuration
	hlsSegmentMaxSize         conf.StringSize
	hlsAllowOrigin            string
	readBufferCount           int
	pathManager               *pathManager
	metrics                   *metrics
	parent                    hlsServerParent

	ctx       context.Context
	ctxCancel func()
	wg        sync.WaitGroup
	ln        net.Listener
	muxers    map[string]*hlsMuxer

	// in
	pathSourceReady chan *path
	request         chan hlsMuxerRequest
	muxerClose      chan *hlsMuxer
	apiMuxersList   chan hlsServerAPIMuxersListReq
}

func newHLSServer(
	parentCtx context.Context,
	address string,
	externalAuthenticationURL string,
	hlsAlwaysRemux bool,
	hlsSegmentCount int,
	hlsSegmentDuration conf.StringDuration,
	hlsSegmentMaxSize conf.StringSize,
	hlsAllowOrigin string,
	readBufferCount int,
	pathManager *pathManager,
	metrics *metrics,
	parent hlsServerParent,
) (*hlsServer, error) {
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}

	ctx, ctxCancel := context.WithCancel(parentCtx)

	s := &hlsServer{
		externalAuthenticationURL: externalAuthenticationURL,
		hlsAlwaysRemux:            hlsAlwaysRemux,
		hlsSegmentCount:           hlsSegmentCount,
		hlsSegmentDuration:        hlsSegmentDuration,
		hlsSegmentMaxSize:         hlsSegmentMaxSize,
		hlsAllowOrigin:            hlsAllowOrigin,
		readBufferCount:           readBufferCount,
		pathManager:               pathManager,
		parent:                    parent,
		metrics:                   metrics,
		ctx:                       ctx,
		ctxCancel:                 ctxCancel,
		ln:                        ln,
		muxers:                    make(map[string]*hlsMuxer),
		pathSourceReady:           make(chan *path),
		request:                   make(chan hlsMuxerRequest),
		muxerClose:                make(chan *hlsMuxer),
		apiMuxersList:             make(chan hlsServerAPIMuxersListReq),
	}

	s.log(logger.Info, "listener opened on "+address)

	s.pathManager.onHLSServerSet(s)

	if s.metrics != nil {
		s.metrics.onHLSServerSet(s)
	}

	s.wg.Add(1)
	go s.run()

	return s, nil
}

// Log is the main logging function.
func (s *hlsServer) log(level logger.Level, format string, args ...interface{}) {
	s.parent.Log(level, "[HLS] "+format, append([]interface{}{}, args...)...)
}

func (s *hlsServer) close() {
	s.log(logger.Info, "listener is closing")
	s.ctxCancel()
	s.wg.Wait()
}

func (s *hlsServer) run() {
	defer s.wg.Done()

	router := gin.New()
	router.NoRoute(s.onRequest)

	hs := &http.Server{Handler: router}
	go hs.Serve(s.ln)

outer:
	for {
		select {
		case pa := <-s.pathSourceReady:
			if s.hlsAlwaysRemux {
				s.findOrCreateMuxer(pa.Name())
			}

		case req := <-s.request:
			r := s.findOrCreateMuxer(req.dir)
			r.onRequest(req)

		case c := <-s.muxerClose:
			if c2, ok := s.muxers[c.PathName()]; !ok || c2 != c {
				continue
			}
			delete(s.muxers, c.PathName())

		case req := <-s.apiMuxersList:
			muxers := make(map[string]*hlsMuxer)

			for name, m := range s.muxers {
				muxers[name] = m
			}

			req.res <- hlsServerAPIMuxersListRes{
				muxers: muxers,
			}

		case <-s.ctx.Done():
			break outer
		}
	}

	s.ctxCancel()

	hs.Shutdown(context.Background())

	s.pathManager.onHLSServerSet(nil)

	if s.metrics != nil {
		s.metrics.onHLSServerSet(nil)
	}
}

func (s *hlsServer) onRequest(ctx *gin.Context) {
	s.log(logger.Info, "[conn %v] %s %s", ctx.ClientIP(), ctx.Request.Method, ctx.Request.URL.Path)

	byts, _ := httputil.DumpRequest(ctx.Request, true)
	s.log(logger.Debug, "[conn %v] [c->s] %s", ctx.ClientIP(), string(byts))

	logw := &httpLogWriter{ResponseWriter: ctx.Writer}
	ctx.Writer = logw

	ctx.Writer.Header().Set("Server", "rtsp-simple-server")
	ctx.Writer.Header().Set("Access-Control-Allow-Origin", s.hlsAllowOrigin)
	ctx.Writer.Header().Set("Access-Control-Allow-Credentials", "true")

	switch ctx.Request.Method {
	case http.MethodGet:

	case http.MethodOptions:
		ctx.Writer.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		ctx.Writer.Header().Set("Access-Control-Allow-Headers", ctx.Request.Header.Get("Access-Control-Request-Headers"))
		ctx.Writer.WriteHeader(http.StatusOK)
		return

	default:
		ctx.Writer.WriteHeader(http.StatusNotFound)
		return
	}

	// remove leading prefix
	pa := ctx.Request.URL.Path[1:]

	switch pa {
	case "", "favicon.ico":
		ctx.Writer.WriteHeader(http.StatusNotFound)
		return
	}

	dir, fname := func() (string, string) {
		if strings.HasSuffix(pa, ".ts") || strings.HasSuffix(pa, ".m3u8") {
			return gopath.Dir(pa), gopath.Base(pa)
		}
		return pa, ""
	}()

	if fname == "" && !strings.HasSuffix(dir, "/") {
		ctx.Writer.Header().Set("Location", "/"+dir+"/")
		ctx.Writer.WriteHeader(http.StatusMovedPermanently)
		return
	}

	dir = strings.TrimSuffix(dir, "/")

	cres := make(chan hlsMuxerResponse)
	hreq := hlsMuxerRequest{
		dir:  dir,
		file: fname,
		req:  ctx.Request,
		res:  cres,
	}

	select {
	case s.request <- hreq:
		res := <-cres

		for k, v := range res.header {
			ctx.Writer.Header().Set(k, v)
		}
		ctx.Writer.WriteHeader(res.status)

		if res.body != nil {
			io.Copy(ctx.Writer, res.body)
		}

	case <-s.ctx.Done():
	}

	s.log(logger.Debug, "[conn %v] [s->c] %s", ctx.ClientIP(), logw.dump())
}

func (s *hlsServer) findOrCreateMuxer(pathName string) *hlsMuxer {
	r, ok := s.muxers[pathName]
	if !ok {
		r = newHLSMuxer(
			s.ctx,
			pathName,
			s.externalAuthenticationURL,
			s.hlsAlwaysRemux,
			s.hlsSegmentCount,
			s.hlsSegmentDuration,
			s.hlsSegmentMaxSize,
			s.readBufferCount,
			&s.wg,
			pathName,
			s.pathManager,
			s)
		s.muxers[pathName] = r
	}
	return r
}

// onMuxerClose is called by hlsMuxer.
func (s *hlsServer) onMuxerClose(c *hlsMuxer) {
	select {
	case s.muxerClose <- c:
	case <-s.ctx.Done():
	}
}

// onPathSourceReady is called by core.
func (s *hlsServer) onPathSourceReady(pa *path) {
	select {
	case s.pathSourceReady <- pa:
	case <-s.ctx.Done():
	}
}

// onAPIHLSMuxersList is called by api.
func (s *hlsServer) onAPIHLSMuxersList(req hlsServerAPIMuxersListReq) hlsServerAPIMuxersListRes {
	req.res = make(chan hlsServerAPIMuxersListRes)
	select {
	case s.apiMuxersList <- req:
		res := <-req.res

		res.data = &hlsServerAPIMuxersListData{
			Items: make(map[string]hlsServerAPIMuxersListItem),
		}

		for _, pa := range res.muxers {
			pa.onAPIHLSMuxersList(hlsServerAPIMuxersListSubReq{data: res.data})
		}

		return res

	case <-s.ctx.Done():
		return hlsServerAPIMuxersListRes{err: fmt.Errorf("terminated")}
	}
}
