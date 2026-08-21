#!/usr/bin/env bash

# ---------------------------------------------------------------------------
# Valide qu'un nœud est prêt pour kubeadm init / kubeadm join.
# Renvoie 0 si tous les contrôles passent, 1 sinon. Leur nombre varie :
# le contrôle des ports devient informatif sur un nœud déjà en cluster.
#
# sudo bash verifier-noeud.sh
# ---------------------------------------------------------------------------

set -uo pipefail

OK=0
KO=0

VERT=$'\033[0;32m'
ROUGE=$'\033[0;31m'
GRIS=$'\033[0;90m'
RESET=$'\033[0m'

# printf %-Ns compte les octets, pas les caractères : les libellés accentués
# seraient décalés. On pad nous-mêmes avec ${#chaine}, qui compte bien les
# caractères en locale UTF-8.

pad() {
    local s="$1"
    local n="$2"

    printf '%s' "$s"

    local i
    for ((i=${#s}; i<n; i++)); do
        printf ' '
    done
}

# check <statut 0|1> <libellé> <valeur>

check() {
    local statut="$1"
    local libelle="$2"
    local valeur="$3"

    if [[ "$statut" -eq 0 ]]; then
        printf '[ %sOK%s ] %s %s\n' \
            "$VERT" \
            "$RESET" \
            "$(pad "$libelle" 24)" \
            "$valeur"

        OK=$((OK + 1))
    else
        printf '[%sFAIL%s] %s %s\n' \
            "$ROUGE" \
            "$RESET" \
            "$(pad "$libelle" 24)" \
            "$valeur"

        KO=$((KO + 1))
    fi
}

info() {
    printf '[%sINFO%s] %s %s\n' \
        "$GRIS" \
        "$RESET" \
        "$(pad "$1" 24)" \
        "$2"
}

echo "=== Vérification du nœud $(hostname) ==="

# --- 1. Distribution --------------------------------------------------------

DISTRO=$(. /etc/os-release && echo "$PRETTY_NAME")

if [[ "$DISTRO" =~ (Ubuntu|Debian|Rocky|AlmaLinux) ]]; then
    s=0
else
    s=1
fi

check "$s" "Distribution" "$DISTRO"

# --- 2. Noyau ---------------------------------------------------------------

# br_netfilter, cgroup v2 et eBPF (Cilium) exigent un noyau >= 5.10.

KVER=$(uname -r)
KMAJ=${KVER%%.*}
KMIN=$(echo "$KVER" | cut -d. -f2)

if (( KMAJ > 5 || (KMAJ == 5 && KMIN >= 10) )); then
    s=0
else
    s=1
fi

check "$s" "Noyau" "$KVER"

# --- 3. Swap ----------------------------------------------------------------

# Le kubelet refuse de démarrer si du swap est actif (failSwapOn par défaut).

NB_SWAP=$(swapon --show --noheadings 2>/dev/null | wc -l)

if [[ "$NB_SWAP" -eq 0 ]]; then
    check 0 "Swap desactive" "aucun swap actif"
else
    check 1 "Swap desactive" \
        "$NB_SWAP swap(s) actif(s) : swapoff -a puis /etc/fstab"
fi

# --- 4 et 5. Modules noyau --------------------------------------------------

# Piège : « lsmod | grep -q » avec `set -o pipefail` renvoie 141 et non 0. grep -q
# sort dès la première correspondance, ferme le tuyau, lsmod reçoit SIGPIPE, et
# pipefail retient ce 141. Le motif est trouvé, et le test échoue quand même.
# On capture d'abord, on cherche ensuite.
MODULES=$(lsmod)

for m in overlay br_netfilter; do
    if grep -q "^${m}\b" <<<"$MODULES"; then
        check 0 "Module $m" "charge"
    else
        check 1 "Module $m" "NON charge : modprobe $m"
    fi
done

# --- 6 à 8. Paramètres sysctl ----------------------------------------------

for p in net.ipv4.ip_forward net.bridge.bridge-nf-call-iptables; do
    V=$(sysctl -n "$p" 2>/dev/null || echo absent)

    if [[ "$V" == "1" ]]; then
        check 0 "${p##*.}" "1"
    else
        check 1 "${p##*.}" "$V (attendu 1)"
    fi
done

V=$(sysctl -n fs.inotify.max_user_instances 2>/dev/null || echo 0)

if [[ "$V" -ge 8192 ]]; then
    check 0 "inotify instances" "$V"
else
    check 1 "inotify instances" "$V (attendu >= 8192)"
fi

# --- 9. containerd ----------------------------------------------------------

ETAT_CTD=$(systemctl is-active containerd 2>/dev/null || echo inactive)

if [[ "$ETAT_CTD" == "active" ]]; then
    check 0 \
        "containerd" \
        "active ($(containerd --version 2>/dev/null | awk '{print $3}'))"
else
    check 1 "containerd" "$ETAT_CTD"
fi

# --- 10. Pilote de cgroup ---------------------------------------------------

# Cause n°1 de nœuds bloqués en NotReady : containerd en cgroupfs alors que le
# kubelet est en systemd. Les deux doivent utiliser systemd.

if grep -q 'SystemdCgroup = true' \
    /etc/containerd/config.toml 2>/dev/null; then

    check 0 "SystemdCgroup" "true"
else
    check 1 \
        "SystemdCgroup" \
        "false ou absent : le kubelet ne demarrera pas"
fi

# --- 11 à 13. Binaires Kubernetes -------------------------------------------

for b in kubelet kubeadm kubectl; do

    if command -v "$b" >/dev/null 2>&1; then

        case "$b" in
            kubelet)
                V=$(kubelet --version 2>/dev/null | awk '{print $2}')
                ;;

            kubeadm)
                V=$(kubeadm version -o short 2>/dev/null)
                ;;

            kubectl)
                V=$(kubectl version --client 2>/dev/null |
                    awk '/Client Version/{print $NF}')
                ;;
        esac

        check 0 "$b installe" "${V:-version inconnue}"

    else
        check 1 "$b installe" "absent"
    fi

done

# --- 14. Ports libres -------------------------------------------------------

# Même piège de tuyau qu'au contrôle des modules : on capture, puis on cherche.
ECOUTES=$(ss -lnt 2>/dev/null | awk '{print $4}')
OCCUPES=""

for p in 6443 10250 2379 2380; do
    if grep -qE "[:.]${p}$" <<<"$ECOUTES"; then
        OCCUPES="$OCCUPES $p"
    fi
done

# Sur un nœud qui a déjà reçu kubeadm init, ces ports sont occupés par le cluster
# lui-même : c'est normal, et ce contrôle n'a plus lieu d'être.
if [[ -f /etc/kubernetes/kubelet.conf ]]; then
    info "Ports libres" "sans objet, le nœud fait déjà partie d'un cluster"
elif [[ -z "$OCCUPES" ]]; then
    check 0 "Ports libres" "6443 10250 2379 2380"
else
    check 1 "Ports libres" "occupes :$OCCUPES"
fi

# --- 15. Résolution DNS et accès au registre d'images ----------------------

if getent hosts registry.k8s.io >/dev/null 2>&1; then
    check 0 "Resolution DNS" "registry.k8s.io resolu"
else
    check 1 "Resolution DNS" "registry.k8s.io injoignable"
fi

# --- 16. Identité du nœud ---------------------------------------------------

# hostname, product_uuid et adresse MAC doivent être uniques dans le cluster.
# Un clone de VM les duplique : kubeadm join échoue ou deux nœuds se recouvrent.

HN=$(hostname)

if [[ "$HN" != "localhost" &&
      "$HN" != "ubuntu" &&
      "$HN" != "debian" ]]; then

    check 0 "Nom d'hote unique" "$HN"
else
    check 1 \
        "Nom d'hote unique" \
        "$HN : renommez la machine"
fi

info "Adresse IP" \
    "$(hostname -I | awk '{print $1}')"

info "product_uuid" \
    "$(cat /sys/class/dmi/id/product_uuid 2>/dev/null || echo indisponible)"

info "Adresse MAC" \
    "$(ip -o link show |
        awk -F': ' '$2!="lo"{print $2; exit}' |
        xargs -I{} cat /sys/class/net/{}/address 2>/dev/null)"

TOTAL=$((OK + KO))

echo "=== ${OK}/${TOTAL} controles reussis ==="

if [[ $KO -eq 0 ]]; then
    echo "Le noeud est pret."
    exit 0
else
    echo "Corrigez les points en FAIL avant de lancer kubeadm." >&2
    exit 1
fi