# pod-terminator
Gracefully shutdown pods that serves `externalTrafficPolicy: local` service, to avoid downtime during deployment.

## Installation
1. Run `make cert-manager` to install cert-manager in the cluster.
2. Run `make install` to install the pod-terminator in `pod-terminator` namespace.

## Usage
Annotate both service and pod with `pod-terminator: enabled`.

Sample in `./deployment/nginx.yaml`
