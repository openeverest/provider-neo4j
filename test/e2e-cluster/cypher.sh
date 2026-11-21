#!/bin/sh
# Runs a Cypher statement on an Instance as the user in its connection Secret
# and prints the result as plain text.
#
# Usage: NAMESPACE=<ns> cypher.sh <instance> <statement>
set -eu
instance=$1
statement=$2
secret=$(kubectl -n "$NAMESPACE" get instance "$instance" -o jsonpath='{.status.connectionSecretRef.name}')
password=$(kubectl -n "$NAMESPACE" get secret "$secret" -o jsonpath='{.data.password}' | base64 -d)
pod=$(kubectl -n "$NAMESPACE" get pod -l "core.openeverest.io/instance=$instance" \
  --field-selector=status.phase=Running -o name | sort | head -n 1)
# neo4j+ssc: TLS with the Instance's own CA, and routing to the cluster's writer.
kubectl -n "$NAMESPACE" exec "$pod" -- \
  cypher-shell -a neo4j+ssc://localhost:7687 -u neo4j -p "$password" --format plain "$statement"
