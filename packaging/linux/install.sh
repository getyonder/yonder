#!/bin/sh
# Puts Yonder in this user's launcher: the program in ~/.local/bin, a desktop
# entry and an icon. No root needed. `./install.sh remove` takes them out
# again (settings and paired devices in ~/.config/yonder are left alone).
set -eu
here=$(cd "$(dirname "$0")" && pwd)
data=${XDG_DATA_HOME:-$HOME/.local/share}
bin=$HOME/.local/bin/yonder
entry=$data/applications/yonder.desktop
icon=$data/icons/hicolor/512x512/apps/yonder.png

if [ "${1:-}" = remove ]; then
	rm -f "$bin" "$entry" "$icon"
	echo "Removed Yonder. If it was set to start at login, also delete ~/.config/autostart/yonder.desktop."
	exit 0
fi

mkdir -p "$(dirname "$bin")" "$(dirname "$entry")" "$(dirname "$icon")"
install -m 755 "$here/yonder" "$bin"
install -m 644 "$here/yonder.png" "$icon"
# The launcher may not have ~/.local/bin on its PATH, so name the program in full.
sed "s|^Exec=.*|Exec=\"$bin\"|" "$here/yonder.desktop" > "$entry"
echo "Installed Yonder: look for it in your launcher, or run $bin"

# A firewall that is on keeps other devices on the network from reaching
# Yonder: it listens on 53317 like LocalSend, or on 53318 when LocalSend has
# that, and hears devices announce themselves on 53317 (UDP). Opening them
# needs the administrator's password, so it is asked first.
ports="53317/tcp 53317/udp 53318/tcp"
if [ "$(systemctl is-active ufw 2>/dev/null)" = active ]; then
	open="" missing=""
	for p in $ports; do
		grep -qs -- "-p ${p#*/} --dport ${p%/*} -j ACCEPT" /etc/ufw/user.rules || missing="$missing $p"
	done
	for p in $missing; do open="$open sudo ufw allow $p;"; done
elif [ "$(systemctl is-active firewalld 2>/dev/null)" = active ]; then
	open="" have=$(firewall-cmd --list-ports 2>/dev/null || true)
	for p in $ports; do
		case " $have " in *" $p "*) ;; *) open="$open sudo firewall-cmd -q --permanent --add-port=$p;" ;; esac
	done
	[ -z "$open" ] || open="$open sudo firewall-cmd -q --reload;"
fi
if [ -n "${open:-}" ]; then
	echo
	echo "This machine's firewall is on. For devices on your network to find Yonder and send to it, these need to be run:"
	echo "$open" | tr ';' '\n' | sed '/^ *$/d; s/^ */  /'
	if [ -t 0 ]; then
		printf "Run them now? It asks for your password. [y/N] "
		read -r answer
		case "$answer" in [yY]*) sh -c "$open" && echo "Opened." ;; *) echo "Left as it is. Paired devices still reach Yonder; run the lines above when you want the rest." ;; esac
	fi
fi
