# Running torrentai on Kubernetes

These manifests run the daemon (`torrentai serve`) as a one-replica
StatefulSet with a volume for its state and downloads.

**Status:** the manifests are checked by a test (`go test
./cmd/torrentai -run TestDeployManifests`) that every `TORRENT_AI_*`
variable is a real flag and that the probe and API ports agree, but
they haven't yet been run in a cluster.

## Install

```sh
docker build -t torrentai:dev .
kind load docker-image torrentai:dev        # or push to your registry and edit image:

kubectl create secret generic torrentai --from-literal=api-token="$(openssl rand -hex 32)"
kubectl apply -f deploy/k8s/torrentai.yaml
kubectl rollout status statefulset/torrentai
```

## Use

```sh
TOKEN=$(kubectl get secret torrentai -o jsonpath='{.data.api-token}' | base64 -d)
kubectl port-forward svc/torrentai-api 8737:8737 &

torrentai add -api-addr 127.0.0.1:8737 -api-token "$TOKEN" 'magnet:?xt=urn:btih:<infohash>'
torrentai ls  -api-addr 127.0.0.1:8737 -api-token "$TOKEN"
```

`TORRENT_AI_API_ADDR` and `TORRENT_AI_API_TOKEN` can replace those flags.

## What's in it

- **Probes.** Readiness is `/readyz`, true once the peer listener is up
  and the DHT routing table has k nodes. It's quick after a restart,
  since the table is saved in the volume. Liveness is `/healthz`.
- **Shutdown.** Kubernetes sends SIGTERM. The daemon announces `stopped`
  to trackers, saves its state and DHT table, uploads a final profile
  and flushes telemetry, then exits with code 143. The 30s grace period
  covers that.
- **Security.** Runs as a non-root user with a read-only root
  filesystem and no capabilities. The TCP API needs the token from the
  Secret.
- **Incoming peers.** The `torrentai-peers` LoadBalancer exposes 6881
  over TCP and UDP. The daemon announces the port it listens on, so the
  outside port must also be 6881: a NodePort's remapping to the 30000+
  range would advertise the wrong port. Without this Service, downloads
  still work through outgoing connections, but fewer peers can reach
  you.
- **Observability.** The commented env vars in the StatefulSet enable
  Pyroscope uploads and OTLP export. A homelab Pyroscope needs only the
  URL; Grafana Cloud also needs the user and password from the Secret.
