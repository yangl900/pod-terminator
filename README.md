# pod-terminator

Run `./deploy.sh` to install the pod-terminator in `pod-terminator` namespace.

Any deployment with annotation `pod-terminator: enabled` will have a pre-termination period.

Sample in `./deployment/nginx.yaml`
