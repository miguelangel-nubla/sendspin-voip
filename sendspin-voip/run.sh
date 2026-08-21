#!/usr/bin/with-contenv bashio

bashio::log.info "Starting Sendspin VoIP Bridge..."

# Launch sendspin-voip binary (will automatically read /data/options.json)
exec /usr/bin/sendspin-voip -config /data/options.json
