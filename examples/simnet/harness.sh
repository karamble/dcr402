#!/usr/bin/env bash
# dcr402 simnet harness: dcrd + voting dcrwallet + two dcrlnd nodes with a
# funded channel, for exercising the dcr402 gate against real Lightning
# payments (M2 acceptance).
#
# Chain bootstrap, wallet seed, and mining sequence are adapted from
# dcrdex's dex/testing/dcr harness (ISC, the Decred ecosystem's canonical
# simnet rig): a prebaked chain snapshot past stake-validation height plus
# the alpha voting wallet keep simnet producing blocks without a slow
# from-genesis ticket bootstrap.
#
# Usage: harness.sh {start|stop|status|e2e|clean}
#   DCR402_SIMNET_ROOT   working dir (default ~/.dcr402-simnet)
#   DCRDEX_TESTING_DCR   dcrdex dex/testing/dcr dir for harnesschain.tar.gz
set -euo pipefail

CMD=${1:-help}
ROOT=${DCR402_SIMNET_ROOT:-$HOME/.dcr402-simnet}
BIN=$ROOT/bin
LOGS=$ROOT/logs
REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
DCRDEX_TESTING_DCR=${DCRDEX_TESTING_DCR:-$HOME/go/src/github.com/decred/dcrdex/dex/testing/dcr}

RPC_USER=user
RPC_PASS=pass
WALLET_PASS=abc
# dcrdex alpha wallet: owns the prebaked chain's tickets, so it must be the
# voter. Public dev constants — simnet only.
ALPHA_SEED=b280922d2cffda44648346412c5ec97f429938105003730414f10b01e1402eac
ALPHA_MINING_ADDR=SsXciQNTo3HuV5tX3yy4hXndRWgLMRVC7Ah

DCRD_P2P=20560
DCRD_RPC=20561
WALLET_RPC=20562
AGENT_WALLET_RPC=20563
AGENT_WALLET_GRPC=20564
SELLER_P2P=20735
SELLER_RPC=127.0.0.1:20009
BUYER_P2P=20736
BUYER_RPC=127.0.0.1:20010
AGENT_HTTP=127.0.0.1:20970
# Fixed API token for the acceptance run: the agent authenticates its whole MCP
# and web surface (it fails closed), so the drivers must present a known token.
AGENT_TOKEN=dcr402-simnet-agent-token

FUND_BUYER_DCR=2.0
FUND_SELLER_DCR=1.0
CHANNEL_ATOMS=10000000 # 0.1 DCR

PATH="$BIN:$HOME/go/bin:$PATH"

log() { printf '\e[1;34m==>\e[0m %s\n' "$*"; }
die() { printf '\e[1;31mxx\e[0m %s\n' "$*" >&2; exit 1; }

dcrd_ctl() {
	dcrctl --simnet --rpcuser=$RPC_USER --rpcpass=$RPC_PASS \
		--rpccert="$ROOT/dcrd/rpc.cert" --rpcserver=127.0.0.1:$DCRD_RPC "$@"
}

wallet_ctl() {
	dcrctl --simnet --wallet --rpcuser=$RPC_USER --rpcpass=$RPC_PASS \
		--rpccert="$ROOT/wallet/rpc.cert" --rpcserver=127.0.0.1:$WALLET_RPC "$@"
}

lncli() { # lncli <seller|buyer> args...
	local node=$1
	shift
	local rpc=$SELLER_RPC
	[ "$node" = buyer ] && rpc=$BUYER_RPC
	dcrlncli --simnet --rpcserver="$rpc" \
		--tlscertpath="$ROOT/$node/tls.cert" \
		--macaroonpath="$ROOT/$node/data/chain/decred/simnet/admin.macaroon" "$@"
}

mine() {
	local n=${1:-1}
	for _ in $(seq "$n"); do
		dcrd_ctl regentemplate >/dev/null
		sleep 0.1
		dcrd_ctl generate 1 >/dev/null
		sleep 0.6 # let the voting wallet see the block and vote
	done
}

wait_for() { # wait_for <what> <tries> <sleep> <cmd...>
	local what=$1 tries=$2 pause=$3
	shift 3
	for _ in $(seq "$tries"); do
		if "$@" >/dev/null 2>&1; then return 0; fi
		sleep "$pause"
	done
	die "timed out waiting for $what"
}

spawn() { # spawn <name> <cmd...>
	local name=$1
	shift
	log "starting $name"
	nohup "$@" >"$LOGS/$name.log" 2>&1 &
	echo $! >"$ROOT/$name.pid"
}

ensure_bins() {
	mkdir -p "$BIN"
	command -v dcrd >/dev/null || die "dcrd not found (expected in ~/go/bin or PATH)"
	command -v dcrwallet >/dev/null || die "dcrwallet not found (expected in ~/go/bin or PATH)"
	if ! command -v dcrctl >/dev/null; then
		log "installing dcrctl into $BIN"
		GOBIN=$BIN go install decred.org/dcrctl@master
	fi
	if ! command -v dcrlnd >/dev/null || ! command -v dcrlncli >/dev/null; then
		local src=$HOME/go/src/github.com/decred/dcrlnd
		[ -d "$src" ] || die "dcrlnd checkout not found at $src"
		log "building dcrlnd + dcrlncli from $src (one-time)"
		(cd "$src" && go build -o "$BIN/dcrlnd" ./cmd/dcrlnd && go build -o "$BIN/dcrlncli" ./cmd/dcrlncli)
	fi
	command -v jq >/dev/null || die "jq is required"
}

start_dcrd() {
	if [ ! -d "$ROOT/dcrd/data/simnet" ]; then
		if [ -f "$DCRDEX_TESTING_DCR/harnesschain.tar.gz" ]; then
			log "seeding chain from dcrdex harnesschain.tar.gz"
			mkdir -p "$ROOT/dcrd/data"
			tar -xzf "$DCRDEX_TESTING_DCR/harnesschain.tar.gz" -C "$ROOT/dcrd/data"
		else
			log "no prebaked chain found — will bootstrap from genesis (slow)"
		fi
	fi
	spawn dcrd dcrd --appdata="$ROOT/dcrd" --simnet \
		--rpcuser=$RPC_USER --rpcpass=$RPC_PASS \
		--rpclisten=127.0.0.1:$DCRD_RPC --listen=127.0.0.1:$DCRD_P2P \
		--miningaddr=$ALPHA_MINING_ADDR --txindex --rejectnonstd \
		--whitelist=127.0.0.0/8 --whitelist=::1 --debuglevel=info
	wait_for dcrd 60 0.5 dcrd_ctl getbestblockhash
	log "dcrd at height $(dcrd_ctl getblockcount)"
}

start_wallet() {
	mkdir -p "$ROOT/wallet"
	cat >"$ROOT/wallet/wallet.conf" <<EOF
simnet=1
nogrpc=1
appdata=$ROOT/wallet
username=$RPC_USER
password=$RPC_PASS
rpclisten=127.0.0.1:$WALLET_RPC
rpccert=$ROOT/wallet/rpc.cert
pass=$WALLET_PASS
rpcconnect=127.0.0.1:$DCRD_RPC
cafile=$ROOT/dcrd/rpc.cert
enablevoting=1
enableticketbuyer=1
ticketbuyer.limit=6
ticketbuyer.balancetomaintainabsolute=1000
EOF
	if [ ! -d "$ROOT/wallet/simnet" ]; then
		log "creating voting wallet from harness seed"
		printf 'y\nn\ny\n%s\n' "$ALPHA_SEED" |
			dcrwallet -C "$ROOT/wallet/wallet.conf" --create >"$LOGS/wallet-create.log" 2>&1
	fi
	spawn wallet dcrwallet -C "$ROOT/wallet/wallet.conf"
	wait_for dcrwallet 120 0.5 wallet_ctl getinfo
	# Give the wallet a moment to finish rescan/ticket discovery, then mine
	# a couple of blocks so everything is fresh.
	sleep 2
	mine 2
	log "wallet ready; spendable: $(wallet_ctl getbalance | jq -r '.balances[0].spendable') DCR"
}

agent_wallet_ctl() {
	dcrctl --simnet --wallet --rpcuser=$RPC_USER --rpcpass=$RPC_PASS \
		--rpccert="$ROOT/agentwallet/rpc.cert" --rpcserver=127.0.0.1:$AGENT_WALLET_RPC "$@"
}

start_agent_wallet() {
	mkdir -p "$ROOT/agentwallet"
	# Client-CA bootstrap: dcrwallet only serves gRPC once a trusted client
	# CA (clients.pem) exists in appdata.
	if [ ! -f "$ROOT/agentwallet/client.pem" ]; then
		log "bootstrapping agent-wallet gRPC client certs"
		(cd "$REPO/examples" && go run ./simnet/walletcert \
			-out "$ROOT/agentwallet" -clients "$ROOT/agentwallet/clients.pem")
	fi
	cat >"$ROOT/agentwallet/wallet.conf" <<EOF
simnet=1
appdata=$ROOT/agentwallet
username=$RPC_USER
password=$RPC_PASS
rpclisten=127.0.0.1:$AGENT_WALLET_RPC
grpclisten=127.0.0.1:$AGENT_WALLET_GRPC
rpccert=$ROOT/agentwallet/rpc.cert
rpckey=$ROOT/agentwallet/rpc.key
clientcafile=$ROOT/agentwallet/clients.pem
pass=$WALLET_PASS
rpcconnect=127.0.0.1:$DCRD_RPC
cafile=$ROOT/dcrd/rpc.cert
EOF
	if [ ! -d "$ROOT/agentwallet/simnet" ]; then
		log "creating fresh-seed agent wallet"
		printf 'y\nn\nn\nOK\n' |
			dcrwallet -C "$ROOT/agentwallet/wallet.conf" --create >"$LOGS/agentwallet-create.log" 2>&1
	fi
	spawn agentwallet dcrwallet -C "$ROOT/agentwallet/wallet.conf"
	wait_for "agent wallet" 120 0.5 agent_wallet_ctl getinfo
	sleep 2
	# Dedicated 'agent' account + funding.
	agent_wallet_ctl createnewaccount agent 2>/dev/null || true
	local agent_addr
	agent_addr=$(agent_wallet_ctl getnewaddress agent)
	log "funding agent wallet account (3 DCR) -> $agent_addr"
	wallet_ctl sendtoaddress "$agent_addr" 3.0 >/dev/null
	mine 2
	wait_for "agent funds" 60 0.5 sh -c \
		"$BIN/dcrctl --simnet --wallet --rpcuser=$RPC_USER --rpcpass=$RPC_PASS \
		 --rpccert='$ROOT/agentwallet/rpc.cert' --rpcserver=127.0.0.1:$AGENT_WALLET_RPC \
		 getbalance agent | jq -e '(.balances[0].spendable|tonumber) > 0' 2>/dev/null || \
		 $BIN/dcrctl --simnet --wallet --rpcuser=$RPC_USER --rpcpass=$RPC_PASS \
		 --rpccert='$ROOT/agentwallet/rpc.cert' --rpcserver=127.0.0.1:$AGENT_WALLET_RPC \
		 getbalance | grep -q ."
	log "agent wallet ready"
}

start_dcrlnd() { # start_dcrlnd <name> <p2p-port> <rpc-addr>
	local name=$1 p2p=$2 rpc=$3
	mkdir -p "$ROOT/$name"
	spawn "$name" dcrlnd --simnet --node=dcrd \
		--dcrd.rpchost=127.0.0.1:$DCRD_RPC --dcrd.rpcuser=$RPC_USER \
		--dcrd.rpcpass=$RPC_PASS --dcrd.rpccert="$ROOT/dcrd/rpc.cert" \
		--datadir="$ROOT/$name/data" --logdir="$ROOT/$name/log" \
		--tlscertpath="$ROOT/$name/tls.cert" --tlskeypath="$ROOT/$name/tls.key" \
		--listen=127.0.0.1:"$p2p" --rpclisten="$rpc" --norest \
		--noseedbackup --debuglevel=info
	wait_for "$name getinfo" 120 0.5 lncli "$name" getinfo
	wait_for "$name chain sync" 240 0.5 sh -c \
		"'$BIN/dcrlncli' --simnet --rpcserver='$rpc' \
		 --tlscertpath='$ROOT/$name/tls.cert' \
		 --macaroonpath='$ROOT/$name/data/chain/decred/simnet/admin.macaroon' \
		 getinfo | jq -e '.synced_to_chain == true'"
	log "$name synced ($(lncli "$name" getinfo | jq -r .identity_pubkey))"
}

fund_and_channel() {
	local buyer_addr seller_addr seller_pubkey
	buyer_addr=$(lncli buyer newaddress p2pkh | jq -r .address)
	seller_addr=$(lncli seller newaddress p2pkh | jq -r .address)
	log "funding buyer ($FUND_BUYER_DCR DCR) and seller ($FUND_SELLER_DCR DCR)"
	wallet_ctl sendtoaddress "$buyer_addr" $FUND_BUYER_DCR >/dev/null
	wallet_ctl sendtoaddress "$seller_addr" $FUND_SELLER_DCR >/dev/null
	mine 2
	wait_for "buyer confirmed funds" 60 0.5 sh -c \
		"'$BIN/dcrlncli' --simnet --rpcserver='$BUYER_RPC' \
		 --tlscertpath='$ROOT/buyer/tls.cert' \
		 --macaroonpath='$ROOT/buyer/data/chain/decred/simnet/admin.macaroon' \
		 walletbalance | jq -e '(.confirmed_balance|tonumber) > 0'"

	seller_pubkey=$(lncli seller getinfo | jq -r .identity_pubkey)
	lncli buyer connect "$seller_pubkey@127.0.0.1:$SELLER_P2P" >/dev/null 2>&1 || true
	if [ "$(lncli buyer listchannels | jq '.channels | length')" = "0" ]; then
		log "opening $CHANNEL_ATOMS-atom channel buyer -> seller"
		lncli buyer openchannel --node_key="$seller_pubkey" --local_amt=$CHANNEL_ATOMS >/dev/null
		mine 6
		wait_for "channel active" 120 0.5 sh -c \
			"'$BIN/dcrlncli' --simnet --rpcserver='$BUYER_RPC' \
			 --tlscertpath='$ROOT/buyer/tls.cert' \
			 --macaroonpath='$ROOT/buyer/data/chain/decred/simnet/admin.macaroon' \
			 listchannels | jq -e '.channels[0].active == true'"
	fi
	log "channel active"

	# Bake the agent's payment-scoped macaroon on the buyer node.
	local agent_mac="$ROOT/agent-payment.macaroon"
	if [ ! -f "$agent_mac" ]; then
		log "baking payment-scoped macaroon on buyer dcrlnd"
		lncli buyer bakemacaroon --save_to "$agent_mac" \
			offchain:read offchain:write invoices:read invoices:write info:read >/dev/null
	fi

	cat >"$ROOT/env.sh" <<EOF
export DCR402_E2E_SELLER_RPC=$SELLER_RPC
export DCR402_E2E_SELLER_TLS=$ROOT/seller/tls.cert
export DCR402_E2E_SELLER_INVOICE_MAC=$ROOT/seller/data/chain/decred/simnet/invoice.macaroon
export DCR402_E2E_SELLER_PUBKEY=$seller_pubkey
export DCR402_E2E_BUYER_RPC=$BUYER_RPC
export DCR402_E2E_BUYER_TLS=$ROOT/buyer/tls.cert
export DCR402_E2E_BUYER_ADMIN_MAC=$ROOT/buyer/data/chain/decred/simnet/admin.macaroon
export DCR402_E2E_MINE="$REPO/examples/simnet/harness.sh mine"

# dcr402-agent inputs (buyer dcrlnd + agent dcrwallet).
export DCR402_AGENT_NETWORK=simnet
export DCR402_AGENT_HTTP_ADDR=$AGENT_HTTP
export DCRLND_RPC=$BUYER_RPC
export DCRLND_TLS_CERT=$ROOT/buyer/tls.cert
export DCRLND_MACAROON=$agent_mac
export DCRWALLET_RPC=127.0.0.1:$AGENT_WALLET_GRPC
export DCRWALLET_TLS_CERT=$ROOT/agentwallet/rpc.cert
export DCRWALLET_CLIENT_CERT=$ROOT/agentwallet/client.pem
export DCRWALLET_CLIENT_KEY=$ROOT/agentwallet/client-key.pem
export DCRWALLET_ACCOUNT=agent
export DCRWALLET_PASSPHRASE_FILE=$ROOT/agentwallet/pass.txt
EOF
	printf '%s' "$WALLET_PASS" >"$ROOT/agentwallet/pass.txt"
	log "environment written to $ROOT/env.sh"
}

cmd_start() {
	mkdir -p "$ROOT" "$LOGS"
	ensure_bins
	start_dcrd
	start_wallet
	start_agent_wallet
	start_dcrlnd seller $SELLER_P2P "$SELLER_RPC"
	start_dcrlnd buyer $BUYER_P2P "$BUYER_RPC"
	fund_and_channel
	log "simnet harness is up. Next: $0 e2e"
}

cmd_e2e() {
	[ -f "$ROOT/env.sh" ] || die "harness not started (no $ROOT/env.sh)"
	# shellcheck disable=SC1091
	source "$ROOT/env.sh"
	log "running dcr402 e2e against live simnet nodes"
	(cd "$REPO/examples" && go run ./simnet/e2e)
}

GATEWAY_LISTEN=127.0.0.1:20980
UPSTREAM_LISTEN=127.0.0.1:20990

cmd_gateway_e2e() {
	[ -f "$ROOT/env.sh" ] || die "harness not started (no $ROOT/env.sh)"
	# shellcheck disable=SC1091
	source "$ROOT/env.sh"

	log "building dcr402d and the toy upstream"
	(cd "$REPO/gateway" && go build -o "$BIN/dcr402d" ./cmd/dcr402d)
	(cd "$REPO/examples" && go build -o "$BIN/simnet-upstream" ./simnet/upstream)

	cat >"$ROOT/dcr402d.yaml" <<EOF
listen: "$GATEWAY_LISTEN"
network: simnet
service: dcr402d-smoke
payto: "$DCR402_E2E_SELLER_PUBKEY"
store: $ROOT/dcr402d.db
ln:
  host: $DCR402_E2E_SELLER_RPC
  tls_cert: $DCR402_E2E_SELLER_TLS
  macaroon: $DCR402_E2E_SELLER_INVOICE_MAC
credits: {enabled: true, onchain: true, confirmations: 1}
topup: {min: "0.001", max: "1", default: "0.005"}
web: {status_page: true}
upstreams:
  - route: /api/
    target: http://$UPSTREAM_LISTEN
    prices: {"GET /api/hello": "0.0025"}
  - route: /mcp
    target: http://$UPSTREAM_LISTEN
    mcp: true
    mode: credits
    tool_price_default: "0.001"
    free_tools: [about]
  - route: /mcp2
    target: http://$UPSTREAM_LISTEN
    mcp: true
    mode: percall
    tool_price_default: "0.0025"
EOF

	spawn upstream "$BIN/simnet-upstream" -listen "$UPSTREAM_LISTEN"
	spawn dcr402d "$BIN/dcr402d" -config "$ROOT/dcr402d.yaml"
	wait_for "dcr402d" 40 0.25 curl -sf "http://$GATEWAY_LISTEN/_dcr402/info"

	log "driving the gateway with real buyer payments"
	set +e
	(cd "$REPO/examples" && DCR402_GW_URL="http://$GATEWAY_LISTEN" go run ./simnet/gateway-e2e)
	rc=$?
	set -e
	for name in dcr402d upstream; do
		if [ -f "$ROOT/$name.pid" ]; then
			kill "$(cat "$ROOT/$name.pid")" 2>/dev/null || true
			rm -f "$ROOT/$name.pid"
		fi
	done
	exit $rc
}

FAC_LISTEN=127.0.0.1:20960

cmd_bazaar_e2e() {
	[ -f "$ROOT/env.sh" ] || die "harness not started (no $ROOT/env.sh)"
	# shellcheck disable=SC1091
	source "$ROOT/env.sh"

	log "building dcrbazaar"
	(cd "$REPO/facilitator" && go build -o "$BIN/dcrbazaar" ./cmd/dcrbazaar)

	cat >"$ROOT/dcrbazaar.yaml" <<EOF
listen: "$FAC_LISTEN"
networks: [simnet]
store: $ROOT/dcrbazaar.db
discovery: {enabled: true, public_submit: true}
api_keys: []
EOF

	spawn dcrbazaar "$BIN/dcrbazaar" -config "$ROOT/dcrbazaar.yaml"
	wait_for "dcrbazaar" 40 0.25 curl -sf "http://$FAC_LISTEN/supported"

	log "driving dcrbazaar with a real buyer payment"
	set +e
	(cd "$REPO/examples" && DCR402_FAC_URL="http://$FAC_LISTEN" go run ./simnet/bazaar-e2e)
	rc=$?
	set -e
	if [ -f "$ROOT/dcrbazaar.pid" ]; then
		kill "$(cat "$ROOT/dcrbazaar.pid")" 2>/dev/null || true
		rm -f "$ROOT/dcrbazaar.pid"
	fi
	exit $rc
}

AGENT_POLICY_PORT=127.0.0.1:20970

# start_agent_stack builds and starts the toy upstream, a dcr402d gateway
# (the fetch_paid target), and dcr402-agent with a demo policy fixture.
# Shared by agent-e2e (which then drives + tears down) and agent-demo (which
# seeds activity and leaves it running).
start_agent_stack() {
	[ -f "$ROOT/env.sh" ] || die "harness not started (no $ROOT/env.sh)"
	# shellcheck disable=SC1091
	source "$ROOT/env.sh"

	log "building dcr402-agent, dcr402d, and the toy upstream"
	(cd "$REPO/agent" && go build -o "$BIN/dcr402-agent" ./cmd/dcr402-agent)
	(cd "$REPO/gateway" && go build -o "$BIN/dcr402d" ./cmd/dcr402d)
	(cd "$REPO/examples" && go build -o "$BIN/simnet-upstream" ./simnet/upstream)

	cat >"$ROOT/dcr402d-agent.yaml" <<EOF
listen: "$GATEWAY_LISTEN"
network: simnet
service: dcr402d-for-agent
payto: "$DCR402_E2E_SELLER_PUBKEY"
store: $ROOT/dcr402d-agent.db
ln:
  host: $DCR402_E2E_SELLER_RPC
  tls_cert: $DCR402_E2E_SELLER_TLS
  macaroon: $DCR402_E2E_SELLER_INVOICE_MAC
credits: {enabled: false}
web: {status_page: true}
upstreams:
  - route: /paid
    target: http://$UPSTREAM_LISTEN
    prices: {"GET /paid": "0.0025"}
EOF
	spawn upstream "$BIN/simnet-upstream" -listen "$UPSTREAM_LISTEN"
	spawn dcr402d "$BIN/dcr402d" -config "$ROOT/dcr402d-agent.yaml"
	wait_for "dcr402d" 40 0.25 curl -sf "http://$GATEWAY_LISTEN/_dcr402/info"

	# default-allow with a 0.5 DCR approval threshold: small payments settle,
	# a 0.6 DCR payment escalates for human approval.
	cat >"$ROOT/agent-policy.yaml" <<EOF
mode: default-allow
limits: {max_per_payment: "1.0", daily_budget: "5.0"}
approval: {threshold: "0.5", ttl: 15m, channel: web}
allow: {domains: ["127.0.0.1"], dcr_addresses: []}
memo_required: true
EOF

	rm -f "$ROOT/agent.db"
	log "starting dcr402-agent (MCP http + web at http://$AGENT_HTTP/)"
	# DCR402_AGENT_HTTP_TOKEN: authenticate the MCP + web surface (fail-closed).
	# DCR402_AGENT_FETCH_ALLOW: fetch_paid blocks loopback by default, so allow
	# the local gateway (a distinct port from the agent's own address, which is
	# refused regardless) as the one paid-fetch target.
	DCR402_AGENT_MCP=http DCR402_AGENT_DB="$ROOT/agent.db" \
		DCR402_AGENT_POLICY="$ROOT/agent-policy.yaml" \
		DCR402_AGENT_HTTP_TOKEN="$AGENT_TOKEN" \
		DCR402_AGENT_FETCH_ALLOW="$GATEWAY_LISTEN" \
		spawn dcr402-agent "$BIN/dcr402-agent"
	wait_for "dcr402-agent" 40 0.25 \
		curl -sf -H "Authorization: Bearer $AGENT_TOKEN" "http://$AGENT_HTTP/api/status"
}

stop_agent_stack() {
	for name in dcr402-agent dcr402d upstream; do
		if [ -f "$ROOT/$name.pid" ]; then
			kill "$(cat "$ROOT/$name.pid")" 2>/dev/null || true
			rm -f "$ROOT/$name.pid"
		fi
	done
}

cmd_agent_e2e() {
	start_agent_stack
	log "driving dcr402-agent over MCP + web API"
	set +e
	(cd "$REPO/examples" && \
		DCR402_AGENT_URL="http://$AGENT_HTTP" \
		DCR402_AGENT_TOKEN="$AGENT_TOKEN" \
		DCR402_GW_PAID_URL="http://$GATEWAY_LISTEN/paid" \
		go run ./simnet/agent-e2e)
	rc=$?
	set -e
	stop_agent_stack
	exit $rc
}

cmd_agent_demo() {
	start_agent_stack
	# Confirm the agent wallet's funding so on-chain sends are spendable.
	mine 2
	# Seed watchable activity; leave the whole stack RUNNING for the browser.
	(cd "$REPO/examples" && \
		DCR402_AGENT_URL="http://$AGENT_HTTP" \
		DCR402_AGENT_TOKEN="$AGENT_TOKEN" \
		DCR402_GW_PAID_URL="http://$GATEWAY_LISTEN/paid" \
		DCR402_E2E_MINE="$REPO/examples/simnet/harness.sh mine" \
		go run ./simnet/agent-demo)
	log "dashboard live at http://$AGENT_HTTP/ — stop with: $0 stop"
}

cmd_status() {
	dcrd_ctl getblockcount >/dev/null 2>&1 || die "dcrd not running"
	echo "height:   $(dcrd_ctl getblockcount)"
	echo "wallet:   $(wallet_ctl getbalance 2>/dev/null | jq -r '.balances[0].spendable') DCR spendable"
	for n in seller buyer; do
		echo "$n: synced=$(lncli $n getinfo 2>/dev/null | jq -r .synced_to_chain) \
channels=$(lncli $n listchannels 2>/dev/null | jq '.channels | length') \
balance=$(lncli $n channelbalance 2>/dev/null | jq -r '.balance')"
	done
}

cmd_stop() {
	for name in dcr402-agent dcr402d upstream buyer seller agentwallet wallet dcrd; do
		if [ -f "$ROOT/$name.pid" ]; then
			pid=$(cat "$ROOT/$name.pid")
			if kill -0 "$pid" 2>/dev/null; then
				log "stopping $name ($pid)"
				kill "$pid" 2>/dev/null || true
			fi
			rm -f "$ROOT/$name.pid"
		fi
	done
	sleep 1
}

case $CMD in
start) cmd_start ;;
stop) cmd_stop ;;
status) cmd_status ;;
e2e) cmd_e2e ;;
gateway-e2e) cmd_gateway_e2e ;;
bazaar-e2e) cmd_bazaar_e2e ;;
agent-e2e) cmd_agent_e2e ;;
agent-demo) cmd_agent_demo ;;
mine) mine "${2:-1}" ;;
clean)
	cmd_stop
	log "removing $ROOT"
	rm -rf "$ROOT"
	;;
*)
	echo "usage: $0 {start|stop|status|e2e|gateway-e2e|bazaar-e2e|agent-e2e|agent-demo|mine [n]|clean}"
	exit 1
	;;
esac
