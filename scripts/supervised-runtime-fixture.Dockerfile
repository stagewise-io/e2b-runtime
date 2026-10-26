# Credential-free composition tests. No production supervisor or runtime code.
FROM node:24.21.0-bookworm-slim@sha256:d6aa754f16b3197301076f047b5def2f02ea1dbbc2ca920407d46d7ec7f87b20 AS fixture-bundle
RUN cp /usr/local/bin/node /orchestrator && cp /usr/local/bin/node /clean-nfs-cache \
    && cd / && sha256sum orchestrator clean-nfs-cache >SHA256SUMS \
    && printf '{"schema_version":1,"commit":"3333333333333333333333333333333333333333","version":"0.16.1+klex.333333333333","os":"linux","arch":"amd64"}\n' >release.json
FROM node:24.21.0-bookworm-slim@sha256:d6aa754f16b3197301076f047b5def2f02ea1dbbc2ca920407d46d7ec7f87b20 AS fixture-supervisor
WORKDIR /app
RUN mkdir vendor && printf 'process.stdout.write("Fixture supervisor started\\n");\n' >main.cjs && touch vendor/info.proto
ENTRYPOINT ["node", "/app/main.cjs"]
