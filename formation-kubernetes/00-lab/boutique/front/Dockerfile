# syntax=docker/dockerfile:1.10
# ---------------------------------------------------------------------------
# boutique-front — NGINX non privilégié servant un front statique
#
#   docker build -t registry.lab.techledger.io/boutique/front:1.0.0 .
# ---------------------------------------------------------------------------

# nginx-unprivileged : même NGINX, mais préconfiguré pour tourner en UID 101
# sans aucune capability. Évite tout le bricolage de chown/chmod habituel.
FROM nginxinc/nginx-unprivileged:1.27-alpine

LABEL org.opencontainers.image.title="boutique-front" \
      org.opencontainers.image.description="Interface web de l'application Boutique" \
      org.opencontainers.image.source="https://gitlab.techledger.io/boutique/front" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="TechLedger"

# Adresse du Service de l'API, substituée au démarrage par l'entrypoint de
# l'image officielle (envsubst sur les fichiers .template).
ENV API_UPSTREAM=boutique-api:8080

# Deux contraintes se combinent ici, et leur solution mérite d'être comprise :
#
#   1. Par défaut, envsubst écrit dans /etc/nginx/conf.d/, qui est inclus À
#      L'INTÉRIEUR du bloc http{}. Notre fichier étant une configuration
#      complète (worker_processes, events, http), il ne peut pas y aller.
#
#   2. Avec readOnlyRootFilesystem: true (module 08) — ou read_only: true en
#      Compose — envsubst ne peut PAS écrire dans /etc/nginx non plus.
#
# Solution : envsubst écrit dans /tmp, qui est un tmpfs inscriptible, et on
# demande explicitement à NGINX de lire sa configuration là-bas.
ENV NGINX_ENVSUBST_OUTPUT_DIR=/tmp

COPY nginx.conf /etc/nginx/templates/nginx.conf.template
COPY index.html /usr/share/nginx/html/index.html

USER 101

EXPOSE 8080

CMD ["nginx", "-c", "/tmp/nginx.conf", "-g", "daemon off;"]
