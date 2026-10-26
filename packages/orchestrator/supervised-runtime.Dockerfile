# Inputs must be cell-local digest references validated by the release planner.
ARG BUNDLE_IMAGE
ARG SUPERVISOR_IMAGE
FROM ${BUNDLE_IMAGE} AS bundle
FROM ${SUPERVISOR_IMAGE}
ARG ASSEMBLY_COMMIT
ARG BUNDLE_COMMIT
ARG SUPERVISOR_COMMIT
LABEL org.opencontainers.image.source="https://github.com/stagewise-io/e2b-runtime" \
      org.opencontainers.image.revision="${ASSEMBLY_COMMIT}" \
      io.stagewise.runtime.commit="${BUNDLE_COMMIT}" \
      io.stagewise.supervisor.commit="${SUPERVISOR_COMMIT}"
COPY --from=bundle /orchestrator /clean-nfs-cache /SHA256SUMS /release.json /opt/e2b/
COPY runtime-release.json verify-supervised-runtime.cjs /opt/e2b/
# Export the pinned Node image's built-in Mozilla CA bundle for Go and preflight.
# node:slim has no system CA bundle. No mutable apt dependency is needed.
RUN mkdir -p /etc/ssl/certs \
    && node -e 'require("node:fs").writeFileSync("/etc/ssl/certs/ca-certificates.crt", require("node:tls").rootCertificates.join("\n") + "\n")' \
    && node /opt/e2b/verify-supervised-runtime.cjs
# Preserve the supervisor's /app working directory, UID and process ownership.
# Verification also prevents chart identity drift at startup. No host installation.
ENTRYPOINT ["/bin/sh", "-ec", "node /opt/e2b/verify-supervised-runtime.cjs && exec node /app/main.cjs"]
