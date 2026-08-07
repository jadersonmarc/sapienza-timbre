# sapienza-timbre — bilheteria descentralizada (Go). Build hermético via vendor/
# (o sapienza-kit é um replace local, então fica vendorizado no repo). Sobe
# independente no Coolify; conecta ao seu PRÓPRIO banco via DATABASE_URL.
FROM golang:1.26-bookworm AS build
WORKDIR /app
COPY . .
# -mod=vendor: sem rede, usa vendor/ (inclui o sapienza-kit).
RUN CGO_ENABLED=0 go build -mod=vendor -trimpath -o /out/timbre ./cmd/server

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/timbre /timbre
EXPOSE 8082
ENTRYPOINT ["/timbre"]
