// Boutique API — application fil rouge de la formation Kubernetes TechLedger.
//
// VERSION TRACEE (module 10, chapitre 10).
// S'ajoute a l'instrumentation du chapitre 07 : les requetes produisent
// maintenant des traces OpenTelemetry, et le contexte de trace voyage jusqu'au
// worker A TRAVERS LA FILE REDIS, pas seulement par des en-tetes HTTP.
//
// VERSION INSTRUMENTEE (module 10, chapitre 07).
// Les metriques ecrites a la main ont ete remplacees par prometheus/client_golang.
// Ce qui change, et pourquoi :
//   - boutique_duree_requete_seconds devient un HISTOGRAM : on peut enfin
//     calculer p50, p95 et p99, ce qu'un compteur de duree ne permettait pas.
//   - le label s'appelle `route` et non plus `chemin`, et il porte le GABARIT
//     de la route, pas le chemin brut.
//   - boutique_requetes_en_vol, un Gauge, mesure la concurrence instantanee.
//   - le collecteur Go standard arrive gratuitement : goroutines, GC, descripteurs.
//
// Cette application est volontairement conçue comme un support pédagogique :
// elle expose des endpoints de diagnostic (/debug/*) qui n'auraient jamais leur
// place en production, mais qui permettent de provoquer à la demande les pannes
// que vous devrez savoir diagnostiquer.
//
// Variables d'environnement :
//
//	PORT              port d'écoute                     (défaut 8080)
//	PG_DSN            chaîne PostgreSQL                 (défaut : voir plus bas)
//	REDIS_ADDR        hôte:port Redis                   (défaut redis:6379)
//	APP_ENV           nom de l'environnement            (défaut "inconnu")
//	MESSAGE_ACCUEIL   message renvoyé sur /             (défaut "Boutique")
//	ARRET_DELAI       délai d'arrêt propre en secondes  (défaut 15)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/codes"
)

// Injectés à la compilation via -ldflags -X (voir Dockerfile).
var (
	version = "dev"
	commit  = "inconnu"
)

// --- Métriques ---------------------------------------------------------------
// Quatre collecteurs, declares une fois. promauto les enregistre tout seul
// aupres du registre par defaut.

var (
	// R + E : un seul Counter porte le debit ET les erreurs, grace au label `code`.
	requetes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "boutique_requetes_total",
		Help: "Nombre total de requetes HTTP traitees.",
	}, []string{"methode", "route", "code"})

	// D : un Histogram, jamais un Summary : seul le premier s'agrege entre Pods.
	// Les seuils encadrent les latences reelles du service : la plupart des
	// routes repondent sous 50 ms, /debug/lent peut monter a plusieurs secondes.
	duree = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "boutique_duree_requete_seconds",
		Help:    "Duree de traitement des requetes HTTP.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"methode", "route"})

	// Gauge : une valeur qui monte et descend. Elle mesure la concurrence,
	// que ni le compteur ni l'histogramme ne donnent.
	enVol = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "boutique_requetes_en_vol",
		Help: "Requetes actuellement en cours de traitement.",
	})

	// La metrique metier : ce qu'aucune sonde d'infrastructure ne peut deviner.
	commandesEmpilees = promauto.NewCounter(prometheus.CounterOpts{
		Name: "boutique_commandes_empilees_total",
		Help: "Commandes poussees dans la file Redis.",
	})

	// Une info statique : la valeur vaut toujours 1, tout est dans les labels.
	buildInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "boutique_build_info",
		Help: "Version de l'application (valeur toujours 1).",
	}, []string{"version", "commit", "go"})
)

// --- Tracage ------------------------------------------------------------------

var traceur = otel.Tracer("boutique-api")

// initTracage branche l'export OTLP et, surtout, le PROPAGATEUR. Sans lui,
// chaque service cree sa propre trace et le lien est perdu en silence.
func initTracage(ctx context.Context, service string) func() {
	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(env("OTEL_ENDPOINT", "tempo.traces.svc.cluster.local:4318")),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		slog.Error("export OTLP impossible", "erreur", err)
		return func() {}
	}
	res, _ := resource.New(ctx, resource.WithAttributes(
		semconv.ServiceName(service),
		semconv.ServiceVersion(version),
		semconv.DeploymentEnvironment(env("APP_ENV", "inconnu")),
	))
	taux, _ := strconv.ParseFloat(env("TAUX_ECHANTILLON", "1.0"), 64)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(taux))),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	return func() {
		ctxA, annuler := context.WithTimeout(context.Background(), 5*time.Second)
		defer annuler()
		_ = tp.Shutdown(ctxA)
	}
}

// --- Application -------------------------------------------------------------

type app struct {
	pg      *pgxpool.Pool
	rdb     *redis.Client
	env     string
	accueil string
	// fuites conserve les allocations de /debug/fuite pour empêcher le GC de
	// les libérer — c'est ainsi qu'on provoque un OOMKill à la demande.
	fuites   [][]byte
	fuitesMu sync.Mutex
	pretDep  atomic.Bool // passe à false pendant l'arrêt propre
}

func env(cle, defaut string) string {
	if v := os.Getenv(cle); v != "" {
		return v
	}
	return defaut
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	buildInfo.WithLabelValues(version, commit, runtime.Version()).Set(1)

	arretTracage := initTracage(context.Background(), env("OTEL_SERVICE_NAME", "boutique-api"))
	defer arretTracage()

	a := &app{
		env:     env("APP_ENV", "inconnu"),
		accueil: env("MESSAGE_ACCUEIL", "Boutique"),
	}
	a.pretDep.Store(true)

	ctx := context.Background()

	// Sous-commande "migrate" : initialise le schéma puis sort.
	// Utilisée par le Job de migration (module 04). Indispensable : l'image
	// étant distroless, il n'y a pas de shell pour faire ce travail
	// en ligne de commande.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		dsn := env("PG_DSN", "postgres://boutique:boutique@postgres:5432/boutique?sslmode=disable")
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			slog.Error("connexion PostgreSQL impossible", "erreur", err)
			os.Exit(1)
		}
		defer pool.Close()
		a.pg = pool
		if err := a.migrer(ctx); err != nil {
			slog.Error("migration en échec", "erreur", err)
			os.Exit(1)
		}
		slog.Info("migration terminée")
		os.Exit(0)
	}

	// PostgreSQL — la connexion est paresseuse : l'application démarre même si
	// la base est absente. C'est volontaire : /readyz doit pouvoir répondre
	// "pas prêt" plutôt que de faire planter le conteneur au démarrage.
	dsn := env("PG_DSN", "postgres://boutique:boutique@postgres:5432/boutique?sslmode=disable")
	if pool, err := pgxpool.New(ctx, dsn); err == nil {
		a.pg = pool
		go a.initSchema()
	} else {
		slog.Error("pool PostgreSQL non créé", "erreur", err)
	}

	a.rdb = redis.NewClient(&redis.Options{Addr: env("REDIS_ADDR", "redis:6379")})

	mux := http.NewServeMux()

	// /metrics n'est PAS instrumente : il ne doit pas se compter lui-meme.
	mux.Handle("GET /metrics", promhttp.Handler())

	// La route est passee en parametre, jamais lue depuis r.URL.Path.
	mux.HandleFunc("GET /", instrumenter("/", a.racine))
	mux.HandleFunc("GET /healthz", instrumenter("/healthz", a.healthz))
	mux.HandleFunc("GET /readyz", instrumenter("/readyz", a.readyz))
	mux.HandleFunc("GET /version", instrumenter("/version", a.versionH))
	mux.HandleFunc("GET /api/produits", instrumenter("/api/produits", a.produits))
	mux.HandleFunc("GET /api/produits/{id}", instrumenter("/api/produits/:id", a.produit))
	mux.HandleFunc("POST /api/commandes", instrumenter("/api/commandes", a.commandes))
	mux.HandleFunc("GET /debug/crash", instrumenter("/debug/crash", a.crash))
	mux.HandleFunc("GET /debug/lent", instrumenter("/debug/lent", a.lent))
	mux.HandleFunc("GET /debug/fuite", instrumenter("/debug/fuite", a.fuite))

	port := env("PORT", "8080")
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// --- Arrêt propre --------------------------------------------------------
	// Séquence indispensable en Kubernetes, détaillée au module 05 :
	//   1. SIGTERM reçu
	//   2. /readyz répond 503 → l'EndpointSlice retire le Pod du Service
	//   3. on attend que les balanceurs propagent le retrait
	//   4. on ferme le serveur en laissant finir les requêtes en cours
	arret := make(chan os.Signal, 1)
	signal.Notify(arret, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		slog.Info("démarrage", "port", port, "version", version,
			"commit", commit, "env", a.env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("échec du serveur", "erreur", err)
			os.Exit(1)
		}
	}()

	sig := <-arret
	slog.Info("signal reçu, début de l'arrêt propre", "signal", sig.String())

	a.pretDep.Store(false) // /readyz bascule en 503

	delai, _ := strconv.Atoi(env("ARRET_DELAI", "15"))
	attente := 5 * time.Second
	slog.Info("retiré des endpoints, attente de propagation", "duree", attente)
	time.Sleep(attente)

	ctxArret, annuler := context.WithTimeout(context.Background(),
		time.Duration(delai)*time.Second)
	defer annuler()

	if err := srv.Shutdown(ctxArret); err != nil {
		slog.Error("arrêt forcé", "erreur", err)
	}
	if a.pg != nil {
		a.pg.Close()
	}
	_ = a.rdb.Close()
	slog.Info("arrêt propre terminé")
}

// instrumenter enveloppe UN handler. La `route` est passee en parametre, jamais
// lue depuis r.URL.Path : ce serait une explosion de cardinalite, chaque
// identifiant creant une serie temporelle distincte.
func instrumenter(route string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		enVol.Inc()
		defer enVol.Dec()

		// Un span racine par requete, nomme d'apres la route normalisee.
		ctx, span := traceur.Start(r.Context(), r.Method+" "+route)
		defer span.End()
		span.SetAttributes(
			attribute.String("http.request.method", r.Method),
			attribute.String("http.route", route),
		)

		debut := time.Now()
		rec := &capteur{ResponseWriter: w, code: 200}
		h(rec, r.WithContext(ctx))

		span.SetAttributes(attribute.Int("http.response.status_code", rec.code))
		if rec.code >= 500 {
			span.SetStatus(codes.Error, "reponse en erreur")
		}

		duree.WithLabelValues(r.Method, route).Observe(time.Since(debut).Seconds())
		requetes.WithLabelValues(r.Method, route, strconv.Itoa(rec.code)).Inc()
	}
}

type capteur struct {
	http.ResponseWriter
	code int
}

func (c *capteur) WriteHeader(code int) {
	c.code = code
	c.ResponseWriter.WriteHeader(code)
}

// --- Handlers ----------------------------------------------------------------

func (a *app) racine(w http.ResponseWriter, r *http.Request) {
	hote, _ := os.Hostname()
	ecrireJSON(w, 200, map[string]any{
		"message": a.accueil,
		"env":     a.env,
		"pod":     hote,
		"version": version,
	})
}

// healthz : le processus est-il vivant ? Ne teste AUCUNE dépendance externe.
// Une liveness probe qui teste la base redémarre tous les Pods quand la base
// tombe — c'est l'erreur la plus coûteuse du module 05.
func (a *app) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(200)
	fmt.Fprintln(w, "ok")
}

// readyz : puis-je servir du trafic ? Teste les dépendances.
func (a *app) readyz(w http.ResponseWriter, r *http.Request) {
	if !a.pretDep.Load() {
		ecrireJSON(w, 503, map[string]any{"pret": false, "raison": "arrêt en cours"})
		return
	}

	ctx, annuler := context.WithTimeout(r.Context(), 2*time.Second)
	defer annuler()

	details := map[string]string{}
	pret := true

	if a.pg == nil {
		details["postgres"] = "pool absent"
		pret = false
	} else if err := a.pg.Ping(ctx); err != nil {
		details["postgres"] = err.Error()
		pret = false
	} else {
		details["postgres"] = "ok"
	}

	if err := a.rdb.Ping(ctx).Err(); err != nil {
		details["redis"] = err.Error()
		pret = false
	} else {
		details["redis"] = "ok"
	}

	code := 200
	if !pret {
		code = 503
	}
	ecrireJSON(w, code, map[string]any{"pret": pret, "dependances": details})
}

func (a *app) versionH(w http.ResponseWriter, r *http.Request) {
	ecrireJSON(w, 200, map[string]any{
		"version": version, "commit": commit, "go": runtime.Version(),
	})
}

func (a *app) initSchema() {
	ctx, annuler := context.WithTimeout(context.Background(), 30*time.Second)
	defer annuler()
	if err := a.migrer(ctx); err != nil {
		slog.Warn("initialisation du schéma impossible", "erreur", err)
		return
	}
	slog.Info("schéma initialisé")
}

// migrer crée le schéma et les données de départ. Volontairement IDEMPOTENTE :
// un Job peut être rejoué à tout moment — sur échec, sur éviction, ou à la
// main. Une migration non rejouable corrompt les données au premier incident.
func (a *app) migrer(ctx context.Context) error {
	// Verrou consultatif : si plusieurs instances migrent en même temps
	// (initContainer répliqué, Job relancé), une seule passe.
	if _, err := a.pg.Exec(ctx, `SELECT pg_advisory_lock(4711)`); err != nil {
		return fmt.Errorf("verrou: %w", err)
	}
	defer a.pg.Exec(context.Background(), `SELECT pg_advisory_unlock(4711)`)

	_, err := a.pg.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS produits (
			id     SERIAL PRIMARY KEY,
			nom    TEXT NOT NULL,
			prix   NUMERIC(10,2) NOT NULL,
			stock  INTEGER NOT NULL DEFAULT 0
		);
		INSERT INTO produits (nom, prix, stock)
		SELECT * FROM (VALUES
			('Clavier mécanique', 89.90, 42),
			('Écran 27 pouces',  249.00, 13),
			('Casque antibruit',  179.50,  7)
		) AS v(nom, prix, stock)
		WHERE NOT EXISTS (SELECT 1 FROM produits);`)
	if err != nil {
		return fmt.Errorf("schéma: %w", err)
	}
	return nil
}

func (a *app) produits(w http.ResponseWriter, r *http.Request) {
	if a.pg == nil {
		ecrireJSON(w, 503, map[string]any{"erreur": "base indisponible"})
		return
	}
	ctx, annuler := context.WithTimeout(r.Context(), 3*time.Second)
	defer annuler()

	ctx, span := traceur.Start(ctx, "postgres.select produits")
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "SELECT"),
		attribute.String("db.collection.name", "produits"),
	)
	lignes, err := a.pg.Query(ctx, `SELECT id, nom, prix, stock FROM produits ORDER BY id`)
	span.End()
	if err != nil {
		slog.Error("requête produits", "erreur", err)
		ecrireJSON(w, 503, map[string]any{"erreur": err.Error()})
		return
	}
	defer lignes.Close()

	type produit struct {
		ID    int     `json:"id"`
		Nom   string  `json:"nom"`
		Prix  float64 `json:"prix"`
		Stock int     `json:"stock"`
	}
	out := []produit{}
	for lignes.Next() {
		var p produit
		if err := lignes.Scan(&p.ID, &p.Nom, &p.Prix, &p.Stock); err != nil {
			continue
		}
		out = append(out, p)
	}
	ecrireJSON(w, 200, out)
}

// produit sert /api/produits/{id}. Son interet ici est pedagogique : c'est la
// route a identifiant variable, celle dont le chemin brut ne doit JAMAIS servir
// de label.
func (a *app) produit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if a.pg == nil {
		ecrireJSON(w, 503, map[string]any{"erreur": "base indisponible"})
		return
	}
	ctx, annuler := context.WithTimeout(r.Context(), 3*time.Second)
	defer annuler()

	var nom string
	var prix float64
	err := a.pg.QueryRow(ctx,
		`SELECT nom, prix FROM produits WHERE id = $1`, id).Scan(&nom, &prix)
	if err != nil {
		ecrireJSON(w, 404, map[string]any{"erreur": "produit inconnu", "id": id})
		return
	}
	ecrireJSON(w, 200, map[string]any{"id": id, "nom": nom, "prix": prix})
}

func (a *app) commandes(w http.ResponseWriter, r *http.Request) {
	var corps map[string]any
	if err := json.NewDecoder(r.Body).Decode(&corps); err != nil {
		ecrireJSON(w, 400, map[string]any{"erreur": "JSON invalide"})
		return
	}
	corps["recue_le"] = time.Now().UTC().Format(time.RFC3339)

	ctx, annuler := context.WithTimeout(r.Context(), 2*time.Second)
	defer annuler()

	// LE point du chapitre : on injecte le contexte de trace DANS LE MESSAGE.
	// Une file n'a pas d'en-tetes HTTP ; le porteur est donc le message lui-meme.
	porteur := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, porteur)
	corps["traceparent"] = porteur.Get("traceparent")

	brut, _ := json.Marshal(corps)

	ctx, span := traceur.Start(ctx, "redis.lpush boutique:commandes")
	span.SetAttributes(
		attribute.String("messaging.system", "redis"),
		attribute.String("messaging.destination.name", "boutique:commandes"),
		attribute.Int("messaging.message.body.size", len(brut)),
	)
	defer span.End()

	if err := a.rdb.LPush(ctx, "boutique:commandes", brut).Err(); err != nil {
		slog.Error("empilement Redis", "erreur", err)
		ecrireJSON(w, 503, map[string]any{"erreur": err.Error()})
		return
	}
	commandesEmpilees.Inc()
	ecrireJSON(w, 202, map[string]any{"statut": "empilée"})
}

// --- Endpoints d'injection de pannes ----------------------------------------
// N'existent que parce que cette application est un support de formation.

// /debug/crash : termine brutalement le processus.
// Utilisé au module 04 pour observer CrashLoopBackOff et le backoff exponentiel.
func (a *app) crash(w http.ResponseWriter, r *http.Request) {
	slog.Error("arrêt brutal demandé via /debug/crash")
	os.Exit(1)
}

// /debug/lent?ms=5000 : répond après N millisecondes.
// Utilisé au module 05 pour faire échouer une liveness probe mal réglée.
func (a *app) lent(w http.ResponseWriter, r *http.Request) {
	ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
	if ms <= 0 {
		ms = 1000
	}
	if ms > 60000 {
		ms = 60000
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
	ecrireJSON(w, 200, map[string]any{"attendu_ms": ms})
}

// /debug/fuite?mo=100 : alloue N Mo et les conserve.
// Utilisé au module 09 pour provoquer un OOMKill et retrouver le code 137.
func (a *app) fuite(w http.ResponseWriter, r *http.Request) {
	mo, _ := strconv.Atoi(r.URL.Query().Get("mo"))
	if mo <= 0 {
		mo = 50
	}
	bloc := make([]byte, mo*1024*1024)
	for i := range bloc { // toucher chaque page, sinon Linux ne l'alloue pas
		bloc[i] = byte(i)
	}
	a.fuitesMu.Lock()
	a.fuites = append(a.fuites, bloc)
	total := len(a.fuites) * mo
	a.fuitesMu.Unlock()

	slog.Warn("fuite mémoire provoquée", "mo_alloues", mo, "mo_total", total)
	ecrireJSON(w, 200, map[string]any{"alloue_mo": mo, "total_mo": total})
}

func ecrireJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
