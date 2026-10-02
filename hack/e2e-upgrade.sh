#!/usr/bin/env bash

# Upgrade e2e test: installs the previous release of the chart,
# creates a StatefulSet with it, switches to the new version, and checks that the
# volumes are adopted and nothing is leaked.
#
# The new version runs as a local controller: the previous release is scaled to 0
# and the tests start the controller built from the source tree. With IMAGE_TAG set,
# the release is upgraded to the local chart and image instead, which also covers
# the chart; the image must be pullable by the cluster (e.g. `kind load`).
#
# The cluster (KUBECONFIG) must already have the backends and the hybrid StorageClass,
# as for `make e2e`.
#
#   IMAGE_REPOSITORY  image of the new version (default ghcr.io/sergelogvinov/hybrid-csi-provisioner)
#   IMAGE_TAG         tag of the new version, empty runs the new version locally
#   OLD_VERSION       chart version of the previous release (default 0.1.12)
#   HELM_VALUES       optional values file for both releases
#   NAMESPACE         namespace of the plugin (default csi-hybrid)
#   RELEASE           helm release name (default hybrid-csi-plugin)

set -euo pipefail

IMAGE_REPOSITORY="${IMAGE_REPOSITORY:-ghcr.io/sergelogvinov/hybrid-csi-provisioner}"
IMAGE_TAG="${IMAGE_TAG:-}"
OLD_VERSION="${OLD_VERSION:-0.1.12}"
NAMESPACE="${NAMESPACE:-csi-hybrid}"
RELEASE="${RELEASE:-hybrid-csi-plugin}"
CHART_REPO="oci://ghcr.io/sergelogvinov/charts/hybrid-csi-plugin"
# The chart has one Deployment, the controller. Only its pods have the component label.
DEPLOYMENT_SELECTOR="app.kubernetes.io/instance=${RELEASE}"
POD_SELECTOR="app.kubernetes.io/instance=${RELEASE},app.kubernetes.io/component=controller"

values=()
if [[ -n "${HELM_VALUES:-}" ]]; then
  values=(-f "${HELM_VALUES}")
fi

run_stage() {
  echo "==> upgrade test, stage $1 (controller $2)"
  E2E_UPGRADE_STAGE="$1" E2E_CONTROLLER="$2" \
    go test -tags=e2e -count=1 -timeout=60m -v -run '^TestUpgrade$' ./test/e2e/upgrade/...
}

echo "==> installing chart ${OLD_VERSION}"
helm upgrade -i -n "${NAMESPACE}" --create-namespace "${RELEASE}" "${CHART_REPO}" \
  --version "${OLD_VERSION}" ${values[@]+"${values[@]}"} --wait

run_stage before external

if [[ -n "${IMAGE_TAG}" ]]; then
  echo "==> upgrading to ${IMAGE_REPOSITORY}:${IMAGE_TAG}"
  helm upgrade -n "${NAMESPACE}" "${RELEASE}" charts/hybrid-csi-plugin \
    --set image.repository="${IMAGE_REPOSITORY}" --set image.tag="${IMAGE_TAG}" ${values[@]+"${values[@]}"} --wait

  run_stage after external
else
  echo "==> scaling the previous release to 0, the new version runs locally"
  kubectl -n "${NAMESPACE}" scale deployment -l "${DEPLOYMENT_SELECTOR}" --replicas=0
  kubectl -n "${NAMESPACE}" wait --for=delete pod -l "${POD_SELECTOR}" --timeout=5m

  run_stage after local

  echo "==> the release ${NAMESPACE}/${RELEASE} is left scaled to 0"
fi
