#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# Remet le cluster dans l'état "fin du module 02" : supprime tout ce que les
# chapitres ont pu créer, sans réinstaller le cluster.
#
#   bash reset-cluster.sh            # demande confirmation
#   bash reset-cluster.sh --force    # sans confirmation
#
# Ce script NE TOUCHE PAS aux namespaces système ni au control plane.
# ---------------------------------------------------------------------------
set -euo pipefail

PROTEGES="default kube-system kube-public kube-node-lease"

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

CTX=$(kubectl config current-context)
echo "Contexte courant : ${CTX}"

if [[ "${1:-}" != "--force" ]]; then
  read -rp "Supprimer tous les objets non-systeme de ce cluster ? (tapez oui) " REP
  [[ "$REP" == "oui" ]] || { echo "Annule."; exit 0; }
fi

# --- 1. Namespaces applicatifs ---------------------------------------------
log "Suppression des namespaces non protégés"
for ns in $(kubectl get ns -o jsonpath='{.items[*].metadata.name}'); do
  if [[ " $PROTEGES " != *" $ns "* ]]; then
    echo "  - $ns"
    kubectl delete ns "$ns" --wait=false >/dev/null
  fi
done

# --- 2. Objets restés dans default -----------------------------------------
log "Nettoyage du namespace default"
kubectl delete all --all -n default --wait=false >/dev/null 2>&1 || true
kubectl delete pvc,configmap,secret,ingress,networkpolicy,serviceaccount \
  --all -n default --field-selector 'metadata.name!=kube-root-ca.crt,metadata.name!=default' \
  --wait=false >/dev/null 2>&1 || true

# --- 3. Objets cluster-scoped créés par les chapitres ----------------------
# On ne supprime que ce qui porte le label de la formation, pour ne pas casser
# les objets système. Tous les manifests du dépôt portent ce label.
log "Suppression des objets cluster-scoped étiquetés formation=k8s-techledger"
for kind in clusterrole clusterrolebinding storageclass \
            persistentvolume validatingwebhookconfiguration \
            mutatingwebhookconfiguration priorityclass ingressclass; do
  kubectl delete "$kind" -l formation=k8s-techledger --wait=false >/dev/null 2>&1 || true
done

# --- 4. PersistentVolumes orphelins ----------------------------------------
log "PersistentVolumes en état Released"
kubectl get pv --no-headers 2>/dev/null \
  | awk '$5=="Released"{print $1}' \
  | xargs -r kubectl delete pv --wait=false >/dev/null 2>&1 || true

# --- 5. Namespaces coincés en Terminating ----------------------------------
# Un namespace reste bloqué quand une CRD a un finalizer et que son contrôleur
# a été supprimé avant les objets. On force en vidant les finalizers.
log "Recherche de namespaces bloqués en Terminating"
sleep 5
for ns in $(kubectl get ns --no-headers 2>/dev/null | awk '$2=="Terminating"{print $1}'); do
  echo "  ! $ns bloqué : suppression des finalizers"
  kubectl get ns "$ns" -o json \
    | jq '.spec.finalizers = []' \
    | kubectl replace --raw "/api/v1/namespaces/${ns}/finalize" -f - >/dev/null
done

# --- 6. État final ----------------------------------------------------------
log "État du cluster"
kubectl get ns
echo
kubectl get nodes
echo
echo "Cluster remis à l'état de sortie du module 02."
