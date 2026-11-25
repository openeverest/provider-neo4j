#!/bin/sh
# Connects to the Instance's Bolt port as <name>, verifying the certificate with
# the ca.crt of the connection Secret.
#
# Usage: NAMESPACE=<ns> verify-tls.sh <instance> <name>
set -eu
instance=$1
name=$2
secret=$(kubectl -n "$NAMESPACE" get instance "$instance" -o jsonpath='{.status.connectionSecretRef.name}')
ca=$(kubectl -n "$NAMESPACE" get secret "$secret" -o jsonpath='{.data.ca\.crt}')
test -n "$ca"
kubectl -n "$NAMESPACE" run "verify-tls-$$" --rm -i --restart=Never --quiet \
  --image=alpine/openssl:3.5.4 --env="CA=$ca" --command -- sh -c "
    echo \"\$CA\" | base64 -d > /tmp/ca.crt
    openssl s_client -connect $instance:7687 -servername $name -verify_hostname $name \
      -CAfile /tmp/ca.crt -verify_return_error </dev/null 2>&1 | grep -E 'Verify return code|Verification'"
