#!/usr/bin/env bash
# Expose /twilio/sms on a public ngrok URL and point the Twilio number's SMS
# webhook at it. The URL changes on every start, so this also rewrites
# LIFESTREAM_PUBLIC_URL in .env: start (or restart) `mise run dev` after the
# URL is printed, since the server checks Twilio signatures against it.
#
# Needs TWILIO_ACCOUNT_SID, TWILIO_AUTH_TOKEN and TWILIO_NUMBER (E.164).
set -euo pipefail

: "${TWILIO_ACCOUNT_SID:?set TWILIO_ACCOUNT_SID in .env}"
: "${TWILIO_AUTH_TOKEN:?set TWILIO_AUTH_TOKEN in .env}"
: "${TWILIO_NUMBER:?set TWILIO_NUMBER in .env}"
addr=${LIFESTREAM_ADDR:-127.0.0.1:8080}

# The policy 404s every path but /twilio/sms: the dashboard has no login.
ngrok http "$addr" --traffic-policy-file scripts/ngrok-policy.yml --log stdout --log-format json &
ngrok_pid=$!
trap 'kill $ngrok_pid 2>/dev/null' EXIT

url=
for _ in $(seq 40); do
	url=$(curl -s localhost:4040/api/tunnels | jq -r '.tunnels[0].public_url // empty' || true)
	[ -n "$url" ] && break
	sleep 0.25
done
[ -n "$url" ] || { echo "tunnel: ngrok reported no URL" >&2; exit 1; }

if grep -q '^LIFESTREAM_PUBLIC_URL=' .env; then
	sed -i "s#^LIFESTREAM_PUBLIC_URL=.*#LIFESTREAM_PUBLIC_URL=$url#" .env
else
	echo "LIFESTREAM_PUBLIC_URL=$url" >>.env
fi

api="https://api.twilio.com/2010-04-01/Accounts/$TWILIO_ACCOUNT_SID/IncomingPhoneNumbers"
auth="$TWILIO_ACCOUNT_SID:$TWILIO_AUTH_TOKEN"
pn=$(curl -fsS -u "$auth" -G "$api.json" --data-urlencode "PhoneNumber=$TWILIO_NUMBER" | jq -r '.incoming_phone_numbers[0].sid // empty')
[ -n "$pn" ] || { echo "tunnel: $TWILIO_NUMBER is not on this Twilio account" >&2; exit 1; }
curl -fsS -u "$auth" "$api/$pn.json" --data-urlencode "SmsUrl=$url/twilio/sms" --data-urlencode "SmsMethod=POST" >/dev/null

echo "tunnel: $TWILIO_NUMBER texts go to $url/twilio/sms; (re)start mise run dev" >&2
wait $ngrok_pid
