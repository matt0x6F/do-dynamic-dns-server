# do-dynamic-dns-server

A simple server that reads your public IP address from Amazon AWS and sets it to the specified address in DigitalOcean DNS. This is useful for folks running a home lab that don't have a static public address.

Features:
- Responds to OS signals
- Fail with errors, retry on next interval
- Environment variable based configuration
- Idempotent requests to DigitalOcean

## Configuration parameters

- `DDNS_DO_API_TOKEN` is the DigitalOcean API token
- `DDNS_DOMAINS` is a comma separated list of domain names to manage
- `DDNS_INTERVAL` is the interval between synchronizations. Can be expressed as `5s`, `15m`, or `20h`. 