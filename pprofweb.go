package main

import (
	"errors"
	"flag"
	"log"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/NYTimes/gziphandler"
	"github.com/google/pprof/driver"
	"github.com/google/uuid"
	"github.com/urfave/cli/v2"
)

const pprofWebPath = "/pprofweb/"

func newServer(listenAddr, baseProfilesPath string) *server {
	return &server{
		listenAddr:       listenAddr,
		baseProfilesPath: baseProfilesPath,
		pprofHandler:     make(map[string]http.Handler),
	}
}

type server struct {
	listenAddr       string
	baseProfilesPath string
	pprofHandler     map[string]http.Handler
}

func (s *server) Run() error {
	return http.ListenAndServe(s.listenAddr, s.handler())
}

func (s *server) startHTTP(args *driver.HTTPServerArgs) error {
	id := args.Host
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
	s.pprofHandler[id] = gziphandler.GzipHandler(mux)
	return nil
}

func (s *server) servePprof(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, pprofWebPath)
	if parts := strings.Split(id, "/"); len(parts) > 0 {
		id = parts[0]
	}

	if handler, ok := s.pprofHandler[id]; ok {
		handler.ServeHTTP(w, r)
		return
	}

	http.Error(w, "profile handler not loaded", http.StatusNotFound)
	return
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

	profile := r.URL.Query().Get("profile")
	if profile == "" {
		w.Write([]byte(rootTemplate))
		return
	}
	profile, err := url.QueryUnescape(profile)
	if err != nil {
		http.Error(w, "could not url decode query param", http.StatusBadRequest)
		return
	}
	profile = filepath.Clean(profile) // prevent a user entering a path like ../../foo
	pprofFilePath := filepath.Join(s.baseProfilesPath, profile)
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

	// start the pprof web handler: pass -http and -no_browser so it starts the
	// handler but does not try to launch a browser
	// our startHTTP will do the appropriate interception
	flags := &pprofFlags{
		args: []string{"-http=" + id + ":0", "-no_browser", pprofFilePath},
	}
	options := &driver.Options{
		Flagset:    flags,
		HTTPServer: s.startHTTP,
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

	// copied from net/http/pprof to avoid relying on the global http.DefaultServeMux
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
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
		},
		Action: func(context *cli.Context) error {
			listenAddr := context.String("listen")
			baseProfilesPath := context.String("profiles")

			s := newServer(listenAddr, baseProfilesPath)
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
