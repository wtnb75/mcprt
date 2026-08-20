FROM alpine:3 AS certs
RUN apk add --no-cache ca-certificates

FROM scratch
ARG TARGETOS
ARG TARGETARCH
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY ${TARGETOS}/${TARGETARCH}/mcprt /mcprt
ENTRYPOINT ["/mcprt"]
