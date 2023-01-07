#!/bin/bash
echo "Adding helm repo"

for ((i=0;i<3;i++)); do
    helm repo add ${helm_repo} ${helm_repo_url} && helm repo update
    [[ $? -eq 0 ]] && break
    echo "helm command failed... retrying $i"
    sleep 2
done
[[ $i == 3 ]] && echo "helm failed" && exit 1

echo "Executing $1"
$1