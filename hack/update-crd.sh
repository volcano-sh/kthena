#!/bin/bash

# This script updates the CRD file to remove specific fields under hostAliases.
# To fix https://github.com/volcano-sh/kthena/issues/680

ROOT_DIR="$(git rev-parse --show-toplevel)"

CRD_FILE="$ROOT_DIR/charts/kthena/charts/workload/crds/workload.serving.volcano.sh_modelservings.yaml"

if [[ ! -f "$CRD_FILE" ]]; then
    echo "Error: CRD file does not exist at $CRD_FILE"
    exit 1
fi

cp "$CRD_FILE" "${CRD_FILE}.backup"
echo "Backup created at ${CRD_FILE}.backup"

awk '
# Track if we are inside the hostAliases field definition
/^ +hostAliases:/             { in_hostAliases=1; hostAliases_indent=length($0)-length(ltrim($0)); print; next }
in_hostAliases==1 && /^ *[a-zA-Z0-9_-]+:/ && length($0) - length(ltrim($0)) <= hostAliases_indent { in_hostAliases=0 }

in_hostAliases==1 {
    if ($1 ~ /^x-kubernetes-list-map-keys:/)     { skip_map_keys=NR; next }
    if ((NR==skip_map_keys+1) && $1 ~ /^-/)      { next }
    if ($1 ~ /^x-kubernetes-list-type:/ && $2 == "map") { next }
}
{ print }

function ltrim(s) { 
    sub(/^[ \t\r\n]+/, "", s);
    return s 
}
' "$CRD_FILE.backup" > "$CRD_FILE"
rm "${CRD_FILE}.backup"
