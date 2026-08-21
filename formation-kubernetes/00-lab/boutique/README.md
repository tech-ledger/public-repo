---
catalogue: oui
---

# Boutique — l'application fil rouge

Cette application est le support pratique de toute la formation. Elle est
déployée dès le module 04 et enrichie jusqu'au module 14.

---

## Architecture

```
                       Internet
                          │
                    ┌─────▼──────┐
                    │  Ingress / │      modules 06 et 14
                    │ Gateway API│
                    └─────┬──────┘
                          │  :80
                    ┌─────▼──────────┐
                    │ boutique-front │  NGINX non privilégié, UID 101
                    │  (statique)    │  relaie /api/* vers l'API
                    └─────┬──────────┘
                          │  :8080
                    ┌─────▼──────────┐
                    │  boutique-api  │  Go, distroless, UID 65532
                    │                │  /metrics, /healthz, /readyz
                    └──┬──────────┬──┘
                       │          │
              ┌────────▼───┐  ┌───▼────────┐      ┌──────────────────┐
              │ PostgreSQL │  │   Redis    │◄─────│ boutique-worker  │
              │  (données) │  │  (file)    │      │  Go, distroless  │
              └────────────┘  └────────────┘      └──────────────────┘
                StatefulSet      cache + file        Deployment, scalable
                 module 04                             modules 04 et 09
```

## Pourquoi cette application et pas un `nginx`

Chaque composant existe pour rendre un concept **observable**.

| Composant | Ce qu'il permet d'enseigner | Module |
|---|---|---|
| `front` statique | Deployment simple, Ingress, TLS, `readOnlyRootFilesystem` | 04, 06, 08 |
| `api` avec dépendances | Readiness contre liveness, échecs en cascade, initContainers | 05 |
| `postgres` | StatefulSet, PVC, `volumeClaimTemplates`, sauvegarde | 04, 07, 14 |
| `redis` | Cache éphémère contre stockage persistant | 07, 14 |
| `worker` sur file | HPA sur métrique non-CPU, KEDA, arrêt propre | 09 |
| `/metrics` | Prometheus, ServiceMonitor, PromQL, alerting | 10 |
| logs JSON | Loki, LogQL, corrélation | 10 |
| `/debug/*` | Injection de pannes reproductibles | 04, 05, 09, 10 |

---

## Les composants

| Répertoire | Langage | Image finale | Taille | Utilisateur |
|---|---|---|---|---|
| [`api/`](api/) | Go 1.26 | `gcr.io/distroless/static` | 12,5 Mo | 65532 |
| [`worker/`](worker/) | Go 1.26 | `gcr.io/distroless/static` | 8,1 Mo | 65532 |
| [`front/`](front/) | HTML + NGINX | `nginx-unprivileged:alpine` | 48,3 Mo | 101 |

Toutes tournent avec `read_only`, `cap_drop: ALL` et un utilisateur non-root —
les contraintes que le module 08 imposera par admission.

---

## Endpoints de l'API

### Fonctionnels

| Méthode | Endpoint | Réponse |
|---|---|---|
| `GET` | `/` | Message d'accueil, environnement, **nom du Pod** |
| `GET` | `/api/produits` | Catalogue, lu dans PostgreSQL |
| `POST` | `/api/commandes` | Empile dans la file Redis, renvoie `202` |
| `GET` | `/version` | Version, commit, version de Go |

Le nom du Pod dans la réponse de `/` est précieux : il permet de **voir la
répartition de charge** entre réplicas d'un simple coup d'œil.

### Opérationnels

| Endpoint | Rôle | Teste les dépendances ? |
|---|---|---|
| `GET /healthz` | Liveness — le processus est-il vivant ? | **Non**, volontairement |
| `GET /readyz` | Readiness — puis-je servir ? | Oui : PostgreSQL et Redis |
| `GET /metrics` | Exposition Prometheus | — |

> **Le point à retenir dès maintenant** : `/healthz` ne teste **aucune**
> dépendance. Une liveness probe qui teste la base de données redémarre tous les
> Pods de l'application quand la base tombe — transformant une panne de base en
> panne totale. Le module 05 y consacre un chapitre entier.

### Injection de pannes

Ces endpoints n'existent que parce que l'application est un support de formation.

| Endpoint | Effet | Utilisé au module |
|---|---|---|
| `GET /debug/crash` | Termine le processus, code 1 | 04 — `CrashLoopBackOff` et backoff exponentiel |
| `GET /debug/lent?ms=5000` | Répond après N ms | 05 — faire échouer une probe mal réglée |
| `GET /debug/fuite?mo=100` | Alloue N Mo et les conserve | 09 — provoquer un `OOMKilled` (code 137) |

---

## Variables d'environnement

### `api`

| Variable | Défaut | Rôle |
|---|---|---|
| `PORT` | `8080` | Port d'écoute |
| `PG_DSN` | `postgres://boutique:boutique@postgres:5432/boutique?sslmode=disable` | Connexion PostgreSQL |
| `REDIS_ADDR` | `redis:6379` | Adresse Redis |
| `APP_ENV` | `inconnu` | Nom de l'environnement, renvoyé par `/` |
| `MESSAGE_ACCUEIL` | `Boutique` | Message de `/` — sert à démontrer les ConfigMap |
| `ARRET_DELAI` | `15` | Secondes accordées à l'arrêt propre |

### `worker`

| Variable | Défaut | Rôle |
|---|---|---|
| `REDIS_ADDR` | `redis:6379` | Adresse Redis |
| `FILE` | `boutique:commandes` | Clé de la file |
| `DUREE_TRAITEMENT` | `250` | Millisecondes par message — règle le débit |
| `ARRET_DELAI` | `20` | Secondes accordées à l'arrêt propre |

### `front`

| Variable | Défaut | Rôle |
|---|---|---|
| `API_UPSTREAM` | `boutique-api:8080` | Service de l'API vers lequel relayer `/api/*` |

---

## Lancer la pile en local, avant Kubernetes

**Faites-le avant le module 04.** Si l'application ne tourne pas ici, elle ne
tournera pas dans Kubernetes — et vous perdriez du temps à chercher un problème
d'orchestration qui n'en est pas un.

```bash
cd 00-lab/boutique
docker compose up -d --build
```

```console
[+] Running 6/6
 ✔ Container boutique-postgres-1  Healthy
 ✔ Container boutique-redis-1     Healthy
 ✔ Container boutique-api-1       Started
 ✔ Container boutique-worker-1    Started
 ✔ Container boutique-worker-2    Started
 ✔ Container boutique-front-1     Started
```

Vérification :

```bash
curl -s localhost:8080/healthz
curl -s localhost:8080/api/produits
```

```console
ok
[{"id":1,"nom":"Clavier mécanique","prix":89.9,"stock":42},
 {"id":2,"nom":"Écran 27 pouces","prix":249,"stock":13},
 {"id":3,"nom":"Casque antibruit","prix":179.5,"stock":7}]
```

Le cycle complet, front → API → Redis → worker :

```bash
for i in 1 2 3 4; do
  curl -s -XPOST localhost:8080/api/commandes -d "{\"produit_id\":$i}" >/dev/null
done
sleep 2
docker compose logs worker | grep -c "commande traitée"
```

```console
4
```

Ouvrez <http://localhost:8080> dans un navigateur : vous voyez le catalogue et le
nom du conteneur qui a servi la requête.

Arrêt :

```bash
docker compose down -v
```

---

## Construire les images

Chaque composant se construit indépendamment :

```bash
VERSION=1.0.0
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo local)
REGISTRE=registry.lab.techledger.io

for c in api worker; do
  docker build -t ${REGISTRE}/boutique/${c}:${VERSION} \
    --build-arg VERSION=${VERSION} \
    --build-arg COMMIT=${COMMIT} \
    ./${c}
done

docker build -t ${REGISTRE}/boutique/front:${VERSION} ./front
```

C'est l'objet du [TP du module 01](https://techledger.io/formations/kubernetes-de-dbutant-expert-matrisez-lorchestration-de-conteneurs/tp-construire-durcir-et-publier-les-images-de-boutique).

---

## Ce que l'application démontre volontairement

Quelques choix qui paraissent étranges et qui sont délibérés.

**`/healthz` ne teste rien.** Voir plus haut. C'est le bon comportement.

**L'API démarre même si PostgreSQL est absent.** Le pool de connexions est
paresseux. Résultat : le Pod démarre, devient `Running`, mais reste `0/1 Ready`
tant que la base ne répond pas. C'est exactement ce qu'il faut : le Pod ne reçoit
aucun trafic, il n'entre pas en `CrashLoopBackOff`, et le message d'erreur est
lisible dans `/readyz`.

**L'arrêt propre attend 5 secondes avant de fermer le serveur.** Cette attente
paraît inutile — elle est indispensable. Quand un Pod est supprimé, Kubernetes
envoie SIGTERM **et** retire le Pod des endpoints **en parallèle**, sans ordre
garanti. Sans cette attente, le serveur ferme avant que les balanceurs aient
propagé le retrait, et des requêtes arrivent sur un port fermé. Module 05.

**Le worker interroge Redis avec un délai de 2 secondes.** Un `BRPop` bloquant
indéfiniment ne verrait jamais le SIGTERM. Le délai court permet de repasser
régulièrement dans la boucle et de réagir immédiatement à l'arrêt.

**Les métriques sont écrites à la main, sans client Prometheus.** Pour que le
module 10 puisse montrer que le format d'exposition n'est que du texte. En
production, utilisez `prometheus/client_golang`.

**Le worker agrège les métriques par motif de route, pas par URL.** Compter
`/api/produits/1`, `/api/produits/2`… créerait une série temporelle par
identifiant. C'est l'explosion de cardinalité qui met Prometheus à genoux —
détaillée au module 10.
