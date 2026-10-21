# fakeplatform: hermetic Polylane registration-API simulator for the dev
# environment and e2e tests. Never shipped.
FROM golang:1.27@sha256:f24164268fc1e741bbdc0066dbadcf1c8426c5a8489f0729c02c3905a53032fd AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/fakeplatform ./cmd/fakeplatform

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/fakeplatform /fakeplatform
EXPOSE 8180
ENTRYPOINT ["/fakeplatform"]
