#!/bin/bash
# Runs TestUnderRouteThatDiverts in a network of its own: two network
# namespaces joined by a cable, and in the first a route that takes the
# cable's network away to another interface, as Tailscale does with an
# accepted subnet route (table 52, a rule before the main table).
# No root is needed, only user namespaces (unshare -Urn), nsenter, ip,
# and python3 for the device at the other end.
#
#   internal/ownroute/testdata/route.sh
set -u
self=$(readlink -f "$0")
cd "$(dirname "$self")/.." || exit 1
if [ "${1:-}" != inside ]; then
	bin=$(mktemp -d) || exit 1
	go test -c -o "$bin/ownroute.test" . && unshare -Urn bash "$self" inside "$bin/ownroute.test"
	status=$?
	rm -r "$bin"
	exit $status
fi
ip link set lo up
unshare -n sleep 600 &
peer=$!
trap 'kill $peer' EXIT
# The second namespace is there when its process has left ours.
until [ "$(readlink /proc/$peer/ns/net)" != "$(readlink /proc/$$/ns/net)" ]; do :; done
ip link add cable0 type veth peer name cable1 netns $peer &&
	ip addr add 10.9.0.1/24 dev cable0 && ip link set cable0 up &&
	nsenter -t $peer -n sh -c 'ip link set lo up && ip addr add 10.9.0.2/24 dev cable1 && ip link set cable1 up' &&
	ip link add away0 type dummy && ip link set away0 up &&
	ip route add 10.9.0.0/24 dev away0 table 52 && ip rule add pref 5270 lookup 52 &&
	sysctl -qw net.ipv4.conf.all.rp_filter=2 net.ipv4.conf.cable0.rp_filter=2 || exit 1
echo "the route: $(ip route get 10.9.0.2 from 10.9.0.1 | head -1)"
YONDER_ROUTE_PEER=10.9.0.2 YONDER_ROUTE_PEER_PID=$peer "$2" -test.run 'TestUnderRouteThatDiverts' -test.v
