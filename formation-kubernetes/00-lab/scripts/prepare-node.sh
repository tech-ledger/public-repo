#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# Prépare un nœud Ubuntu 24.04 ou 26.04 pour Kubernetes v1.36 (kubeadm).
# Idempotent : peut être relancé sans dommage.
#
#   sudo bash prepare-node.sh
#   sudo KUBE_MINOR=v1.35 bash prepare-node.sh      # autre version mineure
# ---------------------------------------------------------------------------
set -euo pipefail

KUBE_MINOR="${KUBE_MINOR:-v1.36}"
KUBE_PATCH="${KUBE_PATCH:-1.36.3-1.1}"

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

[[ $EUID -eq 0 ]] || { echo "Ce script doit être lancé en root." >&2; exit 1; }

# --- 1. Paquets de base ----------------------------------------------------
log "Installation des paquets de base"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq \
  apt-transport-https ca-certificates curl gnupg lsb-release \
  jq vim git wget socat conntrack ethtool ipvsadm nfs-common

# --- 2. Désactivation du swap ----------------------------------------------
# Le kubelet refuse de démarrer si le swap est actif (sauf NodeSwap activé
# explicitement, hors périmètre de cette formation).
log "Désactivation du swap"
swapoff -a
sed -i.bak '/[[:space:]]swap[[:space:]]/s/^\(.*\)$/#\1/' /etc/fstab
# Ubuntu 24.04 cloud peut utiliser zram ou un swapfile systemd
systemctl mask swap.target 2>/dev/null || true

# --- 3. Modules noyau -------------------------------------------------------
# overlay      : système de fichiers union utilisé par containerd
# br_netfilter : fait passer le trafic des bridges par iptables/nftables
log "Chargement des modules noyau"
cat >/etc/modules-load.d/kubernetes.conf <<'EOF'
overlay
br_netfilter
EOF
modprobe overlay
modprobe br_netfilter

# --- 4. Paramètres sysctl ---------------------------------------------------
log "Configuration sysctl"
cat >/etc/sysctl.d/99-kubernetes.conf <<'EOF'
# Routage entre interfaces : indispensable, le nœud route le trafic des Pods
net.ipv4.ip_forward                 = 1
net.ipv6.conf.all.forwarding        = 1

# Le trafic traversant un bridge Linux doit passer par les règles iptables,
# sans quoi les Services et les NetworkPolicy ne s'appliquent pas.
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1

# Limites relevées : un nœud Kubernetes ouvre beaucoup de fichiers et surveille
# beaucoup d'inodes (un watch par conteneur, par volume, par log).
fs.inotify.max_user_instances       = 8192
fs.inotify.max_user_watches         = 524288
fs.file-max                         = 2097152

# Table de suivi de connexions : kube-proxy en crée énormément.
net.netfilter.nf_conntrack_max      = 1048576

# Nombre de mappings mémoire par process (Elasticsearch, JVM, eBPF).
vm.max_map_count                    = 262144
EOF
sysctl --system >/dev/null

# --- 5. containerd ----------------------------------------------------------
log "Installation de containerd"
install -m 0755 -d /etc/apt/keyrings
if [[ ! -f /etc/apt/keyrings/docker.gpg ]]; then
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
    | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
  chmod a+r /etc/apt/keyrings/docker.gpg
fi
cat >/etc/apt/sources.list.d/docker.list <<EOF
deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable
EOF
apt-get update -qq
apt-get install -y -qq containerd.io

log "Configuration de containerd (SystemdCgroup=true)"
mkdir -p /etc/containerd
containerd config default >/etc/containerd/config.toml
# Le kubelet et containerd DOIVENT utiliser le même pilote de cgroup.
# systemd est le pilote par défaut du kubelet depuis la v1.22.
sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
systemctl restart containerd
systemctl enable --now containerd

# --- 6. Dépôt et binaires Kubernetes ---------------------------------------
log "Installation de kubelet, kubeadm et kubectl (${KUBE_PATCH})"
curl -fsSL "https://pkgs.k8s.io/core:/stable:/${KUBE_MINOR}/deb/Release.key" \
  | gpg --dearmor --yes -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
cat >/etc/apt/sources.list.d/kubernetes.list <<EOF
deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] \
https://pkgs.k8s.io/core:/stable:/${KUBE_MINOR}/deb/ /
EOF
apt-get update -qq
apt-get install -y -qq --allow-change-held-packages \
  "kubelet=${KUBE_PATCH}" "kubeadm=${KUBE_PATCH}" "kubectl=${KUBE_PATCH}"

# Le hold empêche un apt upgrade de sauter une version mineure, ce qui
# casserait le cluster. Les upgrades se font au module 12, à la main.
apt-mark hold kubelet kubeadm kubectl

# --- 6b. Image « pause » ----------------------------------------------------
# Elle doit correspondre à celle qu'attend la version de kubelet installée, sinon
# le kubelet la remplace et containerd ramasse la mauvaise. On la demande à kubeadm
# plutôt que de la figer : elle change à presque chaque version mineure.
# Le nom de la clé a changé aussi : containerd 1.x écrivait `sandbox_image = "..."`,
# containerd 2.x (config version 3 et 4) écrit `sandbox = '...'`.
PAUSE="$(kubeadm config images list 2>/dev/null | grep -m1 '/pause:' || true)"
if [[ -n "$PAUSE" ]]; then
  log "Image pause attendue par kubeadm : ${PAUSE}"
  sed -i -E "s#^([[:space:]]*)sandbox_image = .*#\\1sandbox_image = \"${PAUSE}\"#" \
    /etc/containerd/config.toml
  sed -i -E "s#^([[:space:]]*)sandbox = .*#\\1sandbox = '${PAUSE}'#" \
    /etc/containerd/config.toml
  systemctl restart containerd
  grep -qF "$PAUSE" /etc/containerd/config.toml \
    || { echo "L'image pause n'a pas pu être posée dans config.toml" >&2; exit 1; }
fi

systemctl enable kubelet   # il redémarrera en boucle jusqu'à kubeadm init/join

# --- 7. Résumé --------------------------------------------------------------
log "Nœud préparé"
printf '  hostname   : %s\n' "$(hostname)"
printf '  ip         : %s\n' "$(hostname -I | awk '{print $1}')"
printf '  containerd : %s\n' "$(containerd --version | awk '{print $3}')"
printf '  kubeadm    : %s\n' "$(kubeadm version -o short)"
echo
echo "Lancez maintenant verifier-noeud.sh pour valider le nœud."
