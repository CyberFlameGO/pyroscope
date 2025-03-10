package server

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	golog "log"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	contentencoding "github.com/johejo/go-content-encoding"
	"github.com/klauspost/compress/gzhttp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/pyroscope-io/pyroscope/pkg/history"
	"github.com/pyroscope-io/pyroscope/pkg/ingestion"
	"github.com/sirupsen/logrus"
	metrics "github.com/slok/go-http-metrics/metrics/prometheus"
	"github.com/slok/go-http-metrics/middleware"
	"github.com/slok/go-http-metrics/middleware/std"
	"gorm.io/gorm"

	"github.com/pyroscope-io/pyroscope/pkg/api"
	"github.com/pyroscope-io/pyroscope/pkg/api/authz"
	"github.com/pyroscope-io/pyroscope/pkg/api/router"
	"github.com/pyroscope-io/pyroscope/pkg/config"
	"github.com/pyroscope-io/pyroscope/pkg/model"
	"github.com/pyroscope-io/pyroscope/pkg/scrape"
	"github.com/pyroscope-io/pyroscope/pkg/scrape/labels"
	"github.com/pyroscope-io/pyroscope/pkg/server/httputils"
	"github.com/pyroscope-io/pyroscope/pkg/service"
	"github.com/pyroscope-io/pyroscope/pkg/storage"
	"github.com/pyroscope-io/pyroscope/pkg/util/hyperloglog"
	"github.com/pyroscope-io/pyroscope/pkg/util/updates"
	"github.com/pyroscope-io/pyroscope/webapp"
)

//revive:disable:max-public-structs TODO: we will refactor this later

const (
	stateCookieName            = "pyroscopeState"
	gzHTTPCompressionThreshold = 2000
)

type Controller struct {
	drained uint32

	config     *config.Server
	storage    *storage.Storage
	ingestser  ingestion.Ingester
	log        *logrus.Logger
	httpServer *http.Server
	db         *gorm.DB
	notifier   Notifier
	metricsMdw middleware.Middleware
	dir        http.FileSystem

	httpUtils httputils.Utils

	statsMutex sync.Mutex
	stats      map[string]int

	appStats *hyperloglog.HyperLogLogPlus

	// Exported metrics.
	exportedMetrics *prometheus.Registry

	// TODO: Should be moved to a separate Login handler/service.
	authService        service.AuthService
	userService        service.UserService
	jwtTokenService    service.JWTTokenService
	annotationsService service.AnnotationsService
	signupDefaultRole  model.Role

	scrapeManager *scrape.Manager
	historyMgr    history.Manager
}

type Config struct {
	Configuration *config.Server
	Logger        *logrus.Logger
	// TODO(kolesnikovae): Ideally, Storage should be decomposed.
	*storage.Storage
	ingestion.Ingester
	*gorm.DB
	Notifier

	// The registerer is used for exposing server metrics.
	MetricsRegisterer       prometheus.Registerer
	ExportedMetricsRegistry *prometheus.Registry
	ScrapeManager           *scrape.Manager
	HistoryMgr              history.Manager
}

type StatsReceiver interface {
	StatsInc(name string)
}

type Notifier interface {
	// NotificationText returns message that will be displayed to user
	// on index page load. The message should point user to a critical problem.
	// TODO(kolesnikovae): we should poll for notifications (or subscribe).
	NotificationText() string
}

type TargetsResponse struct {
	Job                string              `json:"job"`
	TargetURL          string              `json:"url"`
	DiscoveredLabels   labels.Labels       `json:"discoveredLabels"`
	Labels             labels.Labels       `json:"labels"`
	Health             scrape.TargetHealth `json:"health"`
	LastScrape         time.Time           `json:"lastScrape"`
	LastError          string              `json:"lastError"`
	LastScrapeDuration string              `json:"lastScrapeDuration"`
}

func New(c Config) (*Controller, error) {
	if c.Configuration.BaseURL != "" {
		_, err := url.Parse(c.Configuration.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("BaseURL is invalid: %w", err)
		}
	}

	if c.HistoryMgr == nil {
		c.HistoryMgr = &history.NoopManager{}
	}

	ctrl := Controller{
		config:    c.Configuration,
		log:       c.Logger,
		storage:   c.Storage,
		ingestser: c.Ingester,
		notifier:  c.Notifier,
		stats:     make(map[string]int),
		appStats:  mustNewHLL(),
		httpUtils: httputils.NewDefaultHelper(c.Logger),

		exportedMetrics: c.ExportedMetricsRegistry,
		metricsMdw: middleware.New(middleware.Config{
			Recorder: metrics.NewRecorder(metrics.Config{
				Prefix:   "pyroscope",
				Registry: c.MetricsRegisterer,
			}),
		}),

		db:            c.DB,
		scrapeManager: c.ScrapeManager,
		historyMgr:    c.HistoryMgr,
	}

	var err error
	if ctrl.dir, err = webapp.Assets(); err != nil {
		return nil, err
	}
	if ctrl.signupDefaultRole, err = model.ParseRole(c.Configuration.Auth.SignupDefaultRole); err != nil {
		return nil, fmt.Errorf("default signup role is invalid: %w", err)
	}

	return &ctrl, nil
}

func mustNewHLL() *hyperloglog.HyperLogLogPlus {
	hll, err := hyperloglog.NewPlus(uint8(18))
	if err != nil {
		panic(err)
	}
	return hll
}

func (ctrl *Controller) serverMux() (http.Handler, error) {
	// TODO(kolesnikovae):
	//  - Move mux part to pkg/api/router.
	//  - Make prometheus middleware to support gorilla patterns.
	//  - Make diagnostic endpoints protection configurable.
	//  - Auth middleware should never redirect - the logic should be moved to the client side.
	r := mux.NewRouter()

	r.Use(contentencoding.Decode())

	ctrl.jwtTokenService = service.NewJWTTokenService(
		[]byte(ctrl.config.Auth.JWTSecret),
		24*time.Hour*time.Duration(ctrl.config.Auth.LoginMaximumLifetimeDays))

	ctrl.authService = service.NewAuthService(ctrl.db, ctrl.jwtTokenService)
	ctrl.userService = service.NewUserService(ctrl.db)
	ctrl.annotationsService = service.NewAnnotationsService(ctrl.db)

	apiRouter := router.New(r.PathPrefix("/api").Subrouter(), router.Services{
		Logger:             ctrl.log,
		APIKeyService:      service.NewAPIKeyService(ctrl.db),
		AuthService:        ctrl.authService,
		UserService:        ctrl.userService,
		AnnotationsService: ctrl.annotationsService,
		AdhocService: service.NewAdhocService(
			ctrl.config.MaxNodesRender,
			ctrl.config.AdhocDataPath),
	})

	apiRouter.Use(
		ctrl.drainMiddleware,
		ctrl.authMiddleware(nil))

	if ctrl.isAuthRequired() {
		apiRouter.RegisterUserHandlers()
		apiRouter.RegisterAPIKeyHandlers()
	}
	apiRouter.RegisterAnnotationsHandlers()
	if !ctrl.config.NoAdhocUI {
		apiRouter.RegisterAdhocHandlers()
	}

	// FIXME: not optimal, unify this with the remoteReadHandler at the top
	authorizer := authz.NewAuthorizer(ctrl.log, httputils.NewDefaultHelper(ctrl.log))
	appsRouter := apiRouter.PathPrefix("/apps").Subrouter()
	if ctrl.config.RemoteRead.Enabled {
		h, err := ctrl.remoteReadHandler(ctrl.config.RemoteRead)
		if err != nil {
			logrus.WithError(err).Error("failed to initialize remote read handler")
		} else {
			appsRouter.Methods(http.MethodGet).Handler(h)
			appsRouter.Methods(http.MethodDelete).Handler(h)
		}
	} else {
		appsRouter.Methods(http.MethodGet).Handler(ctrl.getAppsHandler())
		appsRouter.Methods(http.MethodDelete).Handler(authorizer.RequireAdminRole(ctrl.deleteAppsHandler()))
	}

	ingestRouter := r.Path("/ingest").Subrouter()
	ingestRouter.Use(ctrl.drainMiddleware)
	if ctrl.config.Auth.Ingestion.Enabled {
		ingestRouter.Use(
			ctrl.ingestionAuthMiddleware(),
			authz.NewAuthorizer(ctrl.log, httputils.NewDefaultHelper(ctrl.log)).RequireOneOf(
				authz.Role(model.AdminRole),
				authz.Role(model.AgentRole),
			))
	}

	ingestRouter.Methods(http.MethodPost).Handler(ctrl.ingestHandler())

	// Routes not protected with auth. Drained at shutdown.
	insecureRoutes, err := ctrl.getAuthRoutes()
	if err != nil {
		return nil, err
	}

	assetsHandler := r.PathPrefix("/assets/").Handler(http.FileServer(ctrl.dir)).GetHandler().ServeHTTP
	ctrl.addRoutes(r, append(insecureRoutes, []route{
		{"/assets/", assetsHandler}}...),
		ctrl.drainMiddleware)

	// Protected pages:
	// For these routes server responds with 307 and redirects to /login.
	ih := ctrl.indexHandler()
	ctrl.addRoutes(r, []route{
		{"/", ih},
		{"/comparison", ih},
		{"/comparison-diff", ih},
		{"/tracing", ih},
		{"/service-discovery", ih},
		{"/adhoc-single", ih},
		{"/adhoc-comparison", ih},
		{"/adhoc-comparison-diff", ih},
		{"/settings", ih},
		{"/settings/{page}", ih},
		{"/settings/{page}/{subpage}", ih},
		{"/exemplars/single", ih},
		{"/exemplars/merge", ih},
		{"/explore", ih}},
		ctrl.drainMiddleware,
		ctrl.authMiddleware(ctrl.indexHandler()))

	var routes []route
	// TODO(kolesnikovae): Consider implementing a middleware.
	if ctrl.config.RemoteRead.Enabled {
		h, err := ctrl.remoteReadHandler(ctrl.config.RemoteRead)
		if err != nil {
			logrus.WithError(err).Error("failed to initialize remote read handler")
		} else {
			routes = append(routes, []route{
				{"/render", h},
				{"/render-diff", h},
				{"/labels", h},
				{"/label-values", h},
				{"/export", h},
				{"/merge", h},
				{"/api/exemplars:merge", h},
				{"/api/exemplars:query", h},
				// TODO(kolesnikovae): Add adhoc endpoints
			}...)
		}
	} else {
		routes = append(routes, []route{
			{"/render", ctrl.renderHandler()},
			{"/render-diff", ctrl.renderDiffHandler()},
			{"/labels", ctrl.labelsHandler()},
			{"/label-values", ctrl.labelValuesHandler()},
			{"/export", ctrl.exportHandler()},
			{"/merge", ctrl.exemplarsHandler().MergeExemplars},
			{"/api/exemplars:merge", ctrl.exemplarsHandler().MergeExemplars},
			{"/api/exemplars:query", ctrl.exemplarsHandler().QueryExemplars},
		}...)
	}

	// For these routes server responds with 401.
	ctrl.addRoutes(r, routes,
		ctrl.drainMiddleware,
		ctrl.authMiddleware(nil))

	// TODO(kolesnikovae):
	//  Refactor: move mux part to pkg/api/router.
	//  Make prometheus middleware to support gorilla patterns.

	// TODO(kolesnikovae):
	//  Make diagnostic endpoints protection configurable.

	// Diagnostic secure routes: must be protected but not drained.
	diagnosticSecureRoutes := []route{
		{"/config", ctrl.configHandler},
		{"/build", ctrl.buildHandler},
		{"/targets", ctrl.activeTargetsHandler},
		{"/debug/storage/export/{db}", ctrl.storage.DebugExport},
	}
	if !ctrl.config.DisablePprofEndpoint {
		diagnosticSecureRoutes = append(diagnosticSecureRoutes, []route{
			{"/debug/pprof/", pprof.Index},
			{"/debug/pprof/cmdline", pprof.Cmdline},
			{"/debug/pprof/profile", pprof.Profile},
			{"/debug/pprof/symbol", pprof.Symbol},
			{"/debug/pprof/trace", pprof.Trace},
			{"/debug/pprof/allocs", pprof.Index},
			{"/debug/pprof/goroutine", pprof.Index},
			{"/debug/pprof/heap", pprof.Index},
			{"/debug/pprof/threadcreate", pprof.Index},
			{"/debug/pprof/block", pprof.Index},
			{"/debug/pprof/mutex", pprof.Index},
		}...)
	}

	ctrl.addRoutes(r, diagnosticSecureRoutes, ctrl.authMiddleware(nil))
	ctrl.addRoutes(r, []route{
		{"/metrics", promhttp.Handler().ServeHTTP},
		{"/exported-metrics", ctrl.exportedMetricsHandler},
		{"/healthz", ctrl.healthz},
	})

	// Respond with 404 for all other routes.
	r.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		ih(w, r)
	})

	return r, nil
}

func (ctrl *Controller) activeTargetsHandler(w http.ResponseWriter, r *http.Request) {
	targets := ctrl.scrapeManager.TargetsActive()
	resp := []TargetsResponse{}
	for k, v := range targets {
		for _, t := range v {
			var lastError string
			if t.LastError() != nil {
				lastError = t.LastError().Error()
			}
			resp = append(resp, TargetsResponse{
				Job:                k,
				TargetURL:          t.URL().String(),
				DiscoveredLabels:   t.DiscoveredLabels(),
				Labels:             t.Labels(),
				Health:             t.Health(),
				LastScrape:         t.LastScrape(),
				LastError:          lastError,
				LastScrapeDuration: t.LastScrapeDuration().String(),
			})
		}
	}
	ctrl.httpUtils.WriteResponseJSON(r, w, resp)
}

func (ctrl *Controller) exportedMetricsHandler(w http.ResponseWriter, r *http.Request) {
	promhttp.InstrumentMetricHandler(ctrl.exportedMetrics,
		promhttp.HandlerFor(ctrl.exportedMetrics, promhttp.HandlerOpts{})).
		ServeHTTP(w, r)
}

func (ctrl *Controller) getAuthRoutes() ([]route, error) {
	authRoutes := []route{
		{"/login", ctrl.loginHandler},
		{"/logout", ctrl.logoutHandler},
		{"/signup", ctrl.signupHandler},
	}

	if ctrl.config.Auth.Google.Enabled {
		googleHandler, err := newOauthGoogleHandler(ctrl.config.Auth.Google, ctrl.config.BaseURL, ctrl.log)
		if err != nil {
			return nil, err
		}

		authRoutes = append(authRoutes, []route{
			{"/auth/google/login", ctrl.oauthLoginHandler(googleHandler)},
			{"/auth/google/callback", ctrl.callbackHandler(googleHandler.redirectRoute)},
			{"/auth/google/redirect", ctrl.callbackRedirectHandler(googleHandler)},
		}...)
	}

	if ctrl.config.Auth.Github.Enabled {
		githubHandler, err := newGithubHandler(ctrl.config.Auth.Github, ctrl.config.BaseURL, ctrl.log)
		if err != nil {
			return nil, err
		}

		authRoutes = append(authRoutes, []route{
			{"/auth/github/login", ctrl.oauthLoginHandler(githubHandler)},
			{"/auth/github/callback", ctrl.callbackHandler(githubHandler.redirectRoute)},
			{"/auth/github/redirect", ctrl.callbackRedirectHandler(githubHandler)},
		}...)
	}

	if ctrl.config.Auth.Gitlab.Enabled {
		gitlabHandler, err := newOauthGitlabHandler(ctrl.config.Auth.Gitlab, ctrl.config.BaseURL, ctrl.log)
		if err != nil {
			return nil, err
		}

		authRoutes = append(authRoutes, []route{
			{"/auth/gitlab/login", ctrl.oauthLoginHandler(gitlabHandler)},
			{"/auth/gitlab/callback", ctrl.callbackHandler(gitlabHandler.redirectRoute)},
			{"/auth/gitlab/redirect", ctrl.callbackRedirectHandler(gitlabHandler)},
		}...)
	}

	return authRoutes, nil
}

func (ctrl *Controller) getHandler() (http.Handler, error) {
	handler, err := ctrl.serverMux()
	if err != nil {
		return nil, err
	}

	gzhttpMiddleware, err := gzhttp.NewWrapper(gzhttp.MinSize(gzHTTPCompressionThreshold), gzhttp.CompressionLevel(gzip.BestSpeed))
	if err != nil {
		return nil, err
	}

	h := ctrl.corsMiddleware()(gzhttpMiddleware(handler))
	h = ctrl.logginMiddleware(h)

	return h, nil
}

func (ctrl *Controller) Start() error {
	logger := logrus.New()
	w := logger.Writer()
	defer w.Close()
	handler, err := ctrl.getHandler()
	if err != nil {
		return err
	}

	ctrl.httpServer = &http.Server{
		Addr:           ctrl.config.APIBindAddr,
		Handler:        handler,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   15 * time.Second,
		IdleTimeout:    30 * time.Second,
		MaxHeaderBytes: 1 << 20,
		ErrorLog:       golog.New(w, "", 0),
	}

	updates.StartVersionUpdateLoop()

	if ctrl.config.TLSCertificateFile != "" && ctrl.config.TLSKeyFile != "" {
		err = ctrl.httpServer.ListenAndServeTLS(ctrl.config.TLSCertificateFile, ctrl.config.TLSKeyFile)
	} else {
		err = ctrl.httpServer.ListenAndServe()
	}

	// ListenAndServe always returns a non-nil error. After Shutdown or Close,
	// the returned error is ErrServerClosed.
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (ctrl *Controller) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	return ctrl.httpServer.Shutdown(ctx)
}

func (ctrl *Controller) corsMiddleware() mux.MiddlewareFunc {
	if len(ctrl.config.CORS.AllowedOrigins) > 0 {
		options := []handlers.CORSOption{
			handlers.AllowedOrigins(ctrl.config.CORS.AllowedOrigins),
			handlers.AllowedMethods(ctrl.config.CORS.AllowedMethods),
			handlers.AllowedHeaders(ctrl.config.CORS.AllowedHeaders),
			handlers.MaxAge(ctrl.config.CORS.MaxAge),
		}
		if ctrl.config.CORS.AllowCredentials {
			options = append(options, handlers.AllowCredentials())
		}
		return handlers.CORS(options...)
	}
	return func(next http.Handler) http.Handler {
		return next
	}
}

func (ctrl *Controller) Drain() {
	atomic.StoreUint32(&ctrl.drained, 1)
}

func (ctrl *Controller) drainMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadUint32(&ctrl.drained) > 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (ctrl *Controller) trackMetrics(route string) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return std.Handler(route, ctrl.metricsMdw, next)
	}
}

func (ctrl *Controller) redirectPreservingBaseURL(w http.ResponseWriter, r *http.Request, urlStr string, status int) {
	if ctrl.config.BaseURL != "" {
		// we're modifying the URL here so I'm not memoizing it and instead parsing it all over again to create a new object
		u, err := url.Parse(ctrl.config.BaseURL)
		if err != nil {
			// TODO: technically this should never happen because NewController would return an error
			logrus.Error("base URL is invalid, some redirects might not work as expected")
		} else {
			u.Path = filepath.Join(u.Path, urlStr)
			urlStr = u.String()
		}
	}
	http.Redirect(w, r, urlStr, status)
}

func (ctrl *Controller) loginRedirect(w http.ResponseWriter, r *http.Request) {
	ctrl.redirectPreservingBaseURL(w, r, "/login", http.StatusTemporaryRedirect)
}

func (ctrl *Controller) authMiddleware(redirect http.HandlerFunc) mux.MiddlewareFunc {
	if ctrl.isAuthRequired() {
		return api.AuthMiddleware(redirect, ctrl.authService, ctrl.httpUtils)
	}
	return func(next http.Handler) http.Handler {
		return next
	}
}

func (ctrl *Controller) ingestionAuthMiddleware() mux.MiddlewareFunc {
	if ctrl.config.Auth.Ingestion.Enabled {
		asConfig := service.CachingAuthServiceConfig{
			Size: ctrl.config.Auth.Ingestion.CacheSize,
			TTL:  ctrl.config.Auth.Ingestion.CacheTTL,
		}
		as := service.NewCachingAuthService(ctrl.authService, asConfig)
		return api.AuthMiddleware(nil, as, ctrl.httpUtils)
	}
	return func(next http.Handler) http.Handler {
		return next
	}
}

func expectFormats(format string) error {
	switch format {
	case "json", "pprof", "collapsed", "html", "":
		return nil
	default:
		return errUnknownFormat
	}
}

func (ctrl *Controller) logginMiddleware(next http.Handler) http.Handler {
	if ctrl.config.LogLevel == "debug" {
		// log to Stdout using Apache Common Log Format
		// TODO maybe use JSON?
		return handlers.LoggingHandler(os.Stdout, next)
	}

	return next
}
