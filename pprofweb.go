package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/NYTimes/gziphandler"
	"github.com/google/pprof/driver"
	"github.com/google/pprof/profile"
	"github.com/google/uuid"
	"github.com/urfave/cli/v2"
)

const pprofWebPath = "/pprofweb/"

func newServer(listenAddr, baseProfilesPath string, profileValidDuration time.Duration) *server {
	return &server{
		listenAddr:           listenAddr,
		baseProfilesPath:     baseProfilesPath,
		profileValidDuration: profileValidDuration,
		pprofHandler:         make(map[string]*handlerWithExpire),
	}
}

type server struct {
	listenAddr           string
	baseProfilesPath     string
	profileValidDuration time.Duration
	pprofHandler         map[string]*handlerWithExpire
	pprofHandlerMutex    sync.RWMutex
}

type handlerWithExpire struct {
	http.Handler
	timer *time.Timer
}

func (s *server) Run() error {
	return http.ListenAndServe(s.listenAddr, s.logRequest(s.handler()))
}

func (s *server) startHTTP(args *driver.HTTPServerArgs) error {
	id := args.Host
	s.pprofHandlerMutex.Lock()
	defer s.pprofHandlerMutex.Unlock()
	if _, ok := s.pprofHandler[id]; ok {
		return nil
	}

	mux := http.NewServeMux()
	for pattern, handler := range args.Handlers {
		var joinedPattern string
		if pattern == "/" {
			joinedPattern = pprofWebPath + id + "/"
		} else {
			joinedPattern = path.Join(pprofWebPath+id+"/", pattern)
		}
		mux.Handle(joinedPattern, handler)
	}

	// enable gzip compression: flamegraphs can be big!
	handler := gziphandler.GzipHandler(mux)

	timer := time.AfterFunc(time.Second*30, func() {
		s.pprofHandlerMutex.Lock()
		defer s.pprofHandlerMutex.Unlock()
		log.Println("removing", id)
		delete(s.pprofHandler, id)
	})

	s.pprofHandler[id] = &handlerWithExpire{
		Handler: handler,
		timer:   timer,
	}

	return nil
}

func (s *server) servePprof(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, pprofWebPath)
	if parts := strings.Split(id, "/"); len(parts) > 0 {
		id = parts[0]
	}

	s.pprofHandlerMutex.RLock()
	defer s.pprofHandlerMutex.RUnlock()

	if handler, ok := s.pprofHandler[id]; ok {
		handler.timer.Reset(s.profileValidDuration)
		handler.ServeHTTP(w, r)
		return
	}

	http.Error(w, "profile handler not loaded", http.StatusNotFound)
	return
}

func (s *server) logRequest(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s %s\n", r.RemoteAddr, r.Method, r.URL)
		handler.ServeHTTP(w, r)
	})
}

func (s *server) rootHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("rootHandler %s %s", r.Method, r.URL.String())
	if r.Method != http.MethodGet {
		http.Error(w, "wrong method", http.StatusMethodNotAllowed)
		return
	}

	if r.URL.Path != "/" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	profileQueryParam := r.URL.Query().Get("profile")
	if profileQueryParam == "" {
		w.Write([]byte(rootTemplate))
		return
	}
	profileQueryParam, err := url.QueryUnescape(profileQueryParam)
	if err != nil {
		http.Error(w, "could not url decode query param", http.StatusBadRequest)
		return
	}
	profileQueryParam = filepath.Clean(profileQueryParam) // prevent a user entering a path like ../../foo
	pprofFilePath := filepath.Join(s.baseProfilesPath, profileQueryParam)
	if !strings.HasSuffix(pprofFilePath, ".pb.gz") &&
		!strings.HasSuffix(pprofFilePath, ".pb.") {
		http.Error(w, "file extension is not allowed", http.StatusBadRequest)
		return
	}

	if _, err := os.Stat(pprofFilePath); errors.Is(err, os.ErrNotExist) {
		http.Error(w, "profile not found", http.StatusNotFound)
		return
	}

	id := uuid.New().String()

	fetcher := func(src string, duration, timeout time.Duration) (*profile.Profile, string, error) {
		log.Println("fetching", pprofFilePath)
		f, err := os.Open(pprofFilePath)
		if err != nil {
			return nil, "", err
		}
		defer f.Close()
		p, err := profile.Parse(f)
		if err != nil {
			return nil, "", err
		}

		return p, "", nil
	}

	// start the pprof web handler: pass -http and -no_browser so it starts the
	// handler but does not try to launch a browser
	// our startHTTP will do the appropriate interception
	flags := &pprofFlags{
		args: []string{"--http=" + id + ":0", "-no_browser", "--symbolize", "none", ""},
	}
	options := &driver.Options{
		Flagset:    flags,
		HTTPServer: s.startHTTP,
		UI:         &fakeUI{},
		Fetch:      fetcherFn(fetcher),
	}
	if err := driver.PProf(options); err != nil {
		log.Printf("pprof error: %+v", err)
		http.Error(w, "pprof error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, path.Join(pprofWebPath, id), http.StatusSeeOther)
}

// handler returns a handler that servers the pprof web UI.
func (s *server) handler() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.rootHandler)
	mux.HandleFunc(pprofWebPath, s.servePprof)

	// mux.HandleFunc("/debug/pprof/", pprof.Index)
	// mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	// mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	// mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	// mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

func main() {
	a := cli.App{
		Name:        "pprofweb",
		Description: "",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "listen",
				Aliases: []string{"l"},
				Value:   "0.0.0.0:8080",
				Usage:   "",
			},
			&cli.PathFlag{
				Name:  "profiles",
				Value: ".",
				Usage: "base path containing the profiles",
			},
			&cli.DurationFlag{
				Name:  "valid",
				Value: time.Minute * 30,
				Usage: "The generated profile link will be valid for a specific duration. " +
					"Is there is no activity within this duration, the profile will be unloaded so the memory could be released.",
			},
		},
		Action: func(context *cli.Context) error {
			listenAddr := context.String("listen")
			baseProfilesPath := context.String("profiles")
			profileValidDuration := context.Duration("valid")

			s := newServer(listenAddr, baseProfilesPath, profileValidDuration)
			log.Printf("listen on addr %s", listenAddr)
			return s.Run()
		},
	}
	if err := a.Run(os.Args); err != nil {
		panic(err)
	}
}

const rootTemplate = `<!doctype html>
<html>
<head><title>PProf Web Interface</title></head>
<body>
<h1>PProf Web Interface</h1>
<p>View a profile by calling <a href="http://localhost:8080?profile=profile_example.pb.gz">localhost:8080?profile=your_profile_file.pb.gz</a></p>

</body>
</html>
`

// Mostly copied from https://github.com/google/pprof/blob/master/internal/driver/flags.go
type pprofFlags struct {
	args  []string
	s     flag.FlagSet
	usage []string
}

// Bool implements the plugin.FlagSet interface.
func (p *pprofFlags) Bool(o string, d bool, c string) *bool {
	return p.s.Bool(o, d, c)
}

// Int implements the plugin.FlagSet interface.
func (p *pprofFlags) Int(o string, d int, c string) *int {
	return p.s.Int(o, d, c)
}

// Float64 implements the plugin.FlagSet interface.
func (p *pprofFlags) Float64(o string, d float64, c string) *float64 {
	return p.s.Float64(o, d, c)
}

// String implements the plugin.FlagSet interface.
func (p *pprofFlags) String(o, d, c string) *string {
	return p.s.String(o, d, c)
}

// BoolVar implements the plugin.FlagSet interface.
func (p *pprofFlags) BoolVar(b *bool, o string, d bool, c string) {
	p.s.BoolVar(b, o, d, c)
}

// IntVar implements the plugin.FlagSet interface.
func (p *pprofFlags) IntVar(i *int, o string, d int, c string) {
	p.s.IntVar(i, o, d, c)
}

// Float64Var implements the plugin.FlagSet interface.
// the value of the flag.
func (p *pprofFlags) Float64Var(f *float64, o string, d float64, c string) {
	p.s.Float64Var(f, o, d, c)
}

// StringVar implements the plugin.FlagSet interface.
func (p *pprofFlags) StringVar(s *string, o, d, c string) {
	p.s.StringVar(s, o, d, c)
}

// StringList implements the plugin.FlagSet interface.
func (p *pprofFlags) StringList(o, d, c string) *[]*string {
	return &[]*string{p.s.String(o, d, c)}
}

// AddExtraUsage implements the plugin.FlagSet interface.
func (p *pprofFlags) AddExtraUsage(eu string) {
	p.usage = append(p.usage, eu)
}

// ExtraUsage implements the plugin.FlagSet interface.
func (p *pprofFlags) ExtraUsage() string {
	return strings.Join(p.usage, "\n")
}

// Parse implements the plugin.FlagSet interface.
func (p *pprofFlags) Parse(usage func()) []string {
	p.s.Usage = usage
	p.s.Parse(p.args)
	args := p.s.Args()
	if len(args) == 0 {
		usage()
	}
	return args
}

// fakeUI implements pprof's driver.UI.
type fakeUI struct{}

func (*fakeUI) ReadLine(prompt string) (string, error) { return "", io.EOF }

func (*fakeUI) Print(args ...interface{}) {
	msg := fmt.Sprint(args...)
	log.Println(msg)
}

func (*fakeUI) PrintErr(args ...interface{}) {
	msg := fmt.Sprint(args...)
	log.Println(msg)
}

func (*fakeUI) IsTerminal() bool {
	return false
}

func (*fakeUI) WantBrowser() bool {
	return false
}

func (*fakeUI) SetAutoComplete(complete func(string) string) {}

type fetcherFn func(_ string, _, _ time.Duration) (*profile.Profile, string, error)

func (f fetcherFn) Fetch(s string, d, t time.Duration) (*profile.Profile, string, error) {
	return f(s, d, t)
}
