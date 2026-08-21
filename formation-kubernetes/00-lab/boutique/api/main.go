// Boutique API — application fil rouge de la formation Kubernetes TechLedger.
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
	"github.com/redis/go-redis/v9"
)

// Injectés à la compilation via -ldflags -X (voir Dockerfile).
var (
	version = "dev"
	commit  = "inconnu"
)

// --- Métriques ---------------------------------------------------------------
// Volontairement écrites à la main, sans client Prometheus : au module 10 vous
// verrez que le format d'exposition est du texte, rien de plus.

type metriques struct {
	mu             sync.Mutex
	requetes       map[string]uint64 // clé "méthode chemin code"
	dureeTotale    map[string]float64
	commandesEmpil uint64
	demarrage      time.Time
}

func nouvellesMetriques() *metriques {
	return &metriques{
		requetes:    map[string]uint64{},
		dureeTotale: map[string]float64{},
		demarrage:   time.Now(),
	}
}

func (m *metriques) observer(methode, chemin string, code int, duree time.Duration) {
	cle := fmt.Sprintf("%s|%s|%d", methode, chemin, code)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requetes[cle]++
	m.dureeTotale[cle] += duree.Seconds()
}

func (m *metriques) ecrire(w http.ResponseWriter) {
	m.mu.Lock()
	defer m.mu.Unlock()

	fmt.Fprintln(w, "# HELP boutique_build_info Version du binaire en cours d'exécution.")
	fmt.Fprintln(w, "# TYPE boutique_build_info gauge")
	fmt.Fprintf(w, "boutique_build_info{version=%q,commit=%q,go=%q} 1\n",
		version, commit, runtime.Version())

	fmt.Fprintln(w, "# HELP boutique_uptime_secondes Temps écoulé depuis le démarrage.")
	fmt.Fprintln(w, "# TYPE boutique_uptime_secondes gauge")
	fmt.Fprintf(w, "boutique_uptime_secondes %.0f\n", time.Since(m.demarrage).Seconds())

	fmt.Fprintln(w, "# HELP boutique_requetes_total Nombre de requêtes HTTP traitées.")
	fmt.Fprintln(w, "# TYPE boutique_requetes_total counter")
	for cle, n := range m.requetes {
		p := splitCle(cle)
		fmt.Fprintf(w, "boutique_requetes_total{methode=%q,chemin=%q,code=%q} %d\n",
			p[0], p[1], p[2], n)
	}

	fmt.Fprintln(w, "# HELP boutique_duree_secondes_total Temps cumulé passé à traiter les requêtes.")
	fmt.Fprintln(w, "# TYPE boutique_duree_secondes_total counter")
	for cle, d := range m.dureeTotale {
		p := splitCle(cle)
		fmt.Fprintf(w, "boutique_duree_secondes_total{methode=%q,chemin=%q,code=%q} %f\n",
			p[0], p[1], p[2], d)
	}

	fmt.Fprintln(w, "# HELP boutique_commandes_empilees_total Commandes poussées dans la file Redis.")
	fmt.Fprintln(w, "# TYPE boutique_commandes_empilees_total counter")
	fmt.Fprintf(w, "boutique_commandes_empilees_total %d\n",
		atomic.LoadUint64(&m.commandesEmpil))

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Fprintln(w, "# HELP boutique_memoire_octets Mémoire allouée et encore utilisée.")
	fmt.Fprintln(w, "# TYPE boutique_memoire_octets gauge")
	fmt.Fprintf(w, "boutique_memoire_octets %d\n", ms.Alloc)
}

func splitCle(cle string) [3]string {
	var out [3]string
	i, debut := 0, 0
	for j := 0; j < len(cle) && i < 3; j++ {
		if cle[j] == '|' {
			out[i] = cle[debut:j]
			i++
			debut = j + 1
		}
	}
	if i < 3 {
		out[i] = cle[debut:]
	}
	return out
}

// --- Application -------------------------------------------------------------

type app struct {
	pg      *pgxpool.Pool
	rdb     *redis.Client
	met     *metriques
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

	a := &app{
		met:     nouvellesMetriques(),
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
	mux.HandleFunc("GET /", a.racine)
	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("GET /readyz", a.readyz)
	mux.HandleFunc("GET /metrics", a.metrics)
	mux.HandleFunc("GET /version", a.versionH)
	mux.HandleFunc("GET /api/produits", a.produits)
	mux.HandleFunc("POST /api/commandes", a.commandes)
	mux.HandleFunc("GET /debug/crash", a.crash)
	mux.HandleFunc("GET /debug/lent", a.lent)
	mux.HandleFunc("GET /debug/fuite", a.fuite)

	port := env("PORT", "8080")
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           a.instrumenter(mux),
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

// instrumenter enveloppe le mux pour compter les requêtes et leur durée.
func (a *app) instrumenter(suivant http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		debut := time.Now()
		rec := &capteur{ResponseWriter: w, code: 200}
		suivant.ServeHTTP(rec, r)
		// On agrège sur le motif de route, pas sur l'URL brute : sinon chaque
		// identifiant produit une série temporelle distincte et Prometheus
		// explose. C'est l'erreur de cardinalité vue au module 10.
		motif := r.Pattern
		if motif == "" {
			motif = "autre"
		}
		a.met.observer(r.Method, motif, rec.code, time.Since(debut))
	})
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

func (a *app) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	a.met.ecrire(w)
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

	lignes, err := a.pg.Query(ctx, `SELECT id, nom, prix, stock FROM produits ORDER BY id`)
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

func (a *app) commandes(w http.ResponseWriter, r *http.Request) {
	var corps map[string]any
	if err := json.NewDecoder(r.Body).Decode(&corps); err != nil {
		ecrireJSON(w, 400, map[string]any{"erreur": "JSON invalide"})
		return
	}
	corps["recue_le"] = time.Now().UTC().Format(time.RFC3339)
	brut, _ := json.Marshal(corps)

	ctx, annuler := context.WithTimeout(r.Context(), 2*time.Second)
	defer annuler()

	if err := a.rdb.LPush(ctx, "boutique:commandes", brut).Err(); err != nil {
		slog.Error("empilement Redis", "erreur", err)
		ecrireJSON(w, 503, map[string]any{"erreur": err.Error()})
		return
	}
	atomic.AddUint64(&a.met.commandesEmpil, 1)
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
