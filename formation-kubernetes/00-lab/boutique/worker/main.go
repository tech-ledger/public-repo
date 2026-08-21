// Boutique Worker — consommateur asynchrone de la file Redis.
//
// Son rôle pédagogique : fournir une charge de travail dont le volume ne dépend
// PAS du CPU mais de la profondeur d'une file. C'est ce qui permet, au module 09,
// de montrer les limites du HPA sur CPU et l'intérêt de KEDA.
//
// Variables d'environnement :
//
//	REDIS_ADDR       hôte:port Redis                    (défaut redis:6379)
//	FILE             clé de la file                     (défaut boutique:commandes)
//	PORT             port des métriques et probes        (défaut 8080)
//	DUREE_TRAITEMENT millisecondes par message           (défaut 250)
//	ARRET_DELAI      secondes accordées à l'arrêt propre (défaut 20)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	version = "dev"
	commit  = "inconnu"
)

var (
	traites  atomic.Uint64
	echecs   atomic.Uint64
	enCours  atomic.Int64
	demarrag = time.Now()
)

func env(cle, defaut string) string {
	if v := os.Getenv(cle); v != "" {
		return v
	}
	return defaut
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	rdb := redis.NewClient(&redis.Options{Addr: env("REDIS_ADDR", "redis:6379")})
	file := env("FILE", "boutique:commandes")
	duree, _ := strconv.Atoi(env("DUREE_TRAITEMENT", "250"))

	// --- Serveur de probes et de métriques -----------------------------------
	// Un worker n'a pas d'API, mais il DOIT exposer /healthz et /metrics, sinon
	// il est invisible pour Kubernetes et pour Prometheus.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, annuler := context.WithTimeout(r.Context(), 2*time.Second)
		defer annuler()
		if err := rdb.Ping(ctx).Err(); err != nil {
			http.Error(w, "redis injoignable: "+err.Error(), 503)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		ctx, annuler := context.WithTimeout(r.Context(), 2*time.Second)
		defer annuler()
		profondeur, _ := rdb.LLen(ctx, file).Result()

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprintln(w, "# HELP boutique_worker_build_info Version du worker.")
		fmt.Fprintln(w, "# TYPE boutique_worker_build_info gauge")
		fmt.Fprintf(w, "boutique_worker_build_info{version=%q,commit=%q} 1\n", version, commit)

		fmt.Fprintln(w, "# HELP boutique_worker_messages_traites_total Messages traités avec succès.")
		fmt.Fprintln(w, "# TYPE boutique_worker_messages_traites_total counter")
		fmt.Fprintf(w, "boutique_worker_messages_traites_total %d\n", traites.Load())

		fmt.Fprintln(w, "# HELP boutique_worker_echecs_total Messages en échec.")
		fmt.Fprintln(w, "# TYPE boutique_worker_echecs_total counter")
		fmt.Fprintf(w, "boutique_worker_echecs_total %d\n", echecs.Load())

		// LA métrique du module 09 : c'est sur elle que KEDA déclenchera le
		// scaling, parce que le CPU du worker ne reflète pas sa charge réelle.
		fmt.Fprintln(w, "# HELP boutique_file_profondeur Messages en attente dans la file.")
		fmt.Fprintln(w, "# TYPE boutique_file_profondeur gauge")
		fmt.Fprintf(w, "boutique_file_profondeur{file=%q} %d\n", file, profondeur)

		fmt.Fprintln(w, "# HELP boutique_worker_en_cours Messages en cours de traitement.")
		fmt.Fprintln(w, "# TYPE boutique_worker_en_cours gauge")
		fmt.Fprintf(w, "boutique_worker_en_cours %d\n", enCours.Load())

		fmt.Fprintln(w, "# HELP boutique_worker_uptime_secondes Temps depuis le démarrage.")
		fmt.Fprintln(w, "# TYPE boutique_worker_uptime_secondes gauge")
		fmt.Fprintf(w, "boutique_worker_uptime_secondes %.0f\n", time.Since(demarrag).Seconds())
	})

	srv := &http.Server{
		Addr:              ":" + env("PORT", "8080"),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("serveur de métriques", "erreur", err)
		}
	}()

	// --- Boucle de traitement -------------------------------------------------
	ctx, annulerBoucle := context.WithCancel(context.Background())

	arret := make(chan os.Signal, 1)
	signal.Notify(arret, syscall.SIGTERM, syscall.SIGINT)

	fini := make(chan struct{})
	go func() {
		defer close(fini)
		slog.Info("worker démarré", "file", file, "version", version,
			"duree_traitement_ms", duree)

		for {
			// BRPop bloque jusqu'à 2 s. Ce délai court est volontaire : il permet
			// de repasser régulièrement dans la boucle pour tester ctx.Done(),
			// donc de réagir vite à SIGTERM.
			res, err := rdb.BRPop(ctx, 2*time.Second, file).Result()

			select {
			case <-ctx.Done():
				slog.Info("boucle de traitement arrêtée")
				return
			default:
			}

			if errors.Is(err, redis.Nil) {
				continue // file vide, comportement normal
			}
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Warn("lecture de la file impossible", "erreur", err)
				time.Sleep(2 * time.Second) // évite de marteler un Redis en panne
				continue
			}
			if len(res) < 2 {
				continue
			}

			enCours.Add(1)
			time.Sleep(time.Duration(duree) * time.Millisecond) // simule le travail
			enCours.Add(-1)
			traites.Add(1)

			slog.Info("commande traitée",
				"charge", res[1],
				"total_traite", traites.Load())
		}
	}()

	sig := <-arret
	slog.Info("signal reçu, arrêt propre", "signal", sig.String())

	// On arrête d'abord la consommation, PUIS on attend la fin du message en
	// cours. Un worker tué en plein traitement perd le message : c'est
	// exactement ce que terminationGracePeriodSeconds doit couvrir (module 05).
	annulerBoucle()

	delai, _ := strconv.Atoi(env("ARRET_DELAI", "20"))
	select {
	case <-fini:
		slog.Info("boucle terminée proprement")
	case <-time.After(time.Duration(delai) * time.Second):
		slog.Warn("délai d'arrêt dépassé, abandon du message en cours")
	}

	ctxSrv, annulerSrv := context.WithTimeout(context.Background(), 5*time.Second)
	defer annulerSrv()
	_ = srv.Shutdown(ctxSrv)
	_ = rdb.Close()

	slog.Info("worker arrêté", "total_traite", traites.Load(), "echecs", echecs.Load())
}
