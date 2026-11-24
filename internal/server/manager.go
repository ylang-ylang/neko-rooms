package server

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/m1k1o/neko-rooms/internal/config"
	"github.com/m1k1o/neko-rooms/internal/types"
)

type ServerManagerCtx struct {
	logger zerolog.Logger
	router *chi.Mux
	server *http.Server
	config *config.Server
	auth   *adminAuth
}

func New(ApiManager types.ApiManager, roomConfig *config.Room, config *config.Server, proxyHandler http.Handler, agentHandler http.Handler) *ServerManagerCtx {
	logger := log.With().Str("module", "server").Logger()
	auth := newAdminAuth(config)
	// Room/CDP dispatch must not receive owner cookies or cached form-mode
	// Basic credentials, even when the legacy manager prefix is root.
	proxyHandler = auth.withoutOwnerCredentials(proxyHandler)
	agentHandler = auth.withoutOwnerCredentials(agentHandler)

	router := chi.NewRouter()
	router.Use(middleware.RequestID) // Create a request ID for each request

	// get real users ip
	if config.Proxy {
		router.Use(middleware.RealIP)
	}

	// add http logger
	router.Use(middleware.RequestLogger(&logformatter{logger}))
	router.Use(middleware.Recoverer) // Recover from panics without crashing server

	// Basic CORS
	if config.CORS {
		// for more ideas, see: https://developer.github.com/v3/#cross-origin-resource-sharing
		router.Use(cors.Handler(cors.Options{
			AllowOriginFunc: func(r *http.Request, origin string) bool {
				if auth.mode == "form" && (auth.isAPIPath(r.URL.Path) || auth.isAuthPath(r.URL.Path) ||
					(auth.prefix != "" && (r.URL.Path == auth.prefix || strings.HasPrefix(r.URL.Path, auth.root())))) {
					return auth.sameOrigin(r, origin, false)
				}
				return true
			},
			AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
			AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
			AllowCredentials: true,
			MaxAge:           300, // Maximum value not ignored by any of major browsers
		}))
	}

	//
	// admin page
	//

	protected := auth.protect

	// DEPRECATED: admin should not be served from the same path as rooms
	if auth.prefix == "" {
		// cache static file paths
		staticFiles := map[string]struct{}{}
		if config.Admin.Static != "" {
			filepath.Walk(config.Admin.Static,
				func(p string, info os.FileInfo, err error) error {
					if err != nil {
						return err
					}
					staticFiles[path.Clean(p)] = struct{}{}
					return nil
				})
		}

		// serve static files
		router.Use(func(next http.Handler) http.Handler {
			// if static files are disabled
			if config.Admin.Static == "" {
				return next
			}

			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Authentication endpoints remain public even if a static file
				// happens to share their name under the deprecated root prefix.
				if auth.isAuthPath(r.URL.Path) {
					next.ServeHTTP(w, r)
					return
				}
				filePath := path.Join(config.Admin.Static, r.URL.Path)

				// check if file exists to serve it
				if _, ok := staticFiles[filePath]; ok {
					// serve protected assets
					protected(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						http.ServeFile(w, r, filePath)
					})).ServeHTTP(w, r)
					return
				}

				next.ServeHTTP(w, r)
			})
		})
	}

	// Register public auth endpoints before the protected UI catch-all.
	auth.mount(router)
	if auth.prefix == "" {
		// serve protected API
		router.With(protected).Route("/api", ApiManager.Mount)
	} else {
		router.With(protected).Get(auth.prefix, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, auth.root(), http.StatusSeeOther)
		})
		router.With(protected).Route(auth.root(), func(r chi.Router) {
			// serve protected API
			r.Route("/api", ApiManager.Mount)

			// serve static files
			r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
				filePath := chi.URLParam(r, "*")
				if filePath == "" {
					filePath = "index.html"
				}
				filePath = filepath.Clean(filePath)
				filePath = filepath.Join(config.Admin.Static, filePath)
				if _, err := os.Stat(filePath); err == nil {
					http.ServeFile(w, r, filePath)
				} else if errors.Is(err, fs.ErrNotExist) {
					http.NotFound(w, r)
				} else {
					http.Error(w, err.Error(), http.StatusInternalServerError)
				}
			})
		})
	}

	// mount pprof endpoint
	if config.PProf {
		router.Mount("/debug", middleware.Profiler())
		logger.Info().Msgf("with pprof endpoint")
	}

	// mount metrics prometheus endpoint
	if config.Metrics {
		router.Mount("/metrics", promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}))
		logger.Info().Msgf("with metrics endpoint")
	}

	// Reserve only CDP endpoints: existing room UI/signaling may also live
	// under /rooms/. Keep the full wildcard for the Agent's upstream path.
	router.HandleFunc("/rooms/{roomName}/*", func(w http.ResponseWriter, r *http.Request) {
		rest := chi.URLParam(r, "*")
		if rest == "json" || strings.HasPrefix(rest, "json/") || strings.HasPrefix(rest, "devtools/") {
			agentHandler.ServeHTTP(w, r)
			return
		}
		proxyHandler.ServeHTTP(w, r)
	})
	router.Handle("/rooms/{roomName}", proxyHandler)

	// handle all remaining paths with proxy
	router.Handle("/*", proxyHandler)

	return &ServerManagerCtx{
		logger: logger,
		router: router,
		server: &http.Server{
			Addr:    config.Bind,
			Handler: router,
		},
		config: config,
		auth:   auth,
	}
}

func (s *ServerManagerCtx) Start() {
	if s.config.Cert != "" && s.config.Key != "" {
		go func() {
			if err := s.server.ListenAndServeTLS(s.config.Cert, s.config.Key); err != http.ErrServerClosed {
				s.logger.Panic().Err(err).Msg("unable to start https server")
			}
		}()
		s.logger.Info().Msgf("https listening on %s", s.server.Addr)
	} else {
		go func() {
			if err := s.server.ListenAndServe(); err != http.ErrServerClosed {
				s.logger.Panic().Err(err).Msg("unable to start http server")
			}
		}()
		s.logger.Info().Msgf("http listening on %s", s.server.Addr)
	}
}

func (s *ServerManagerCtx) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return s.server.Shutdown(ctx)
}
