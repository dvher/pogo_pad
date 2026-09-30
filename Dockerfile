FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/pogo-pad ./cmd/pogo-pad

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/pogo-pad /usr/local/bin/pogo-pad
ENV POGO_DB=/data/pogo-pad.db POGO_ADDR=:8080
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["pogo-pad"]
CMD ["serve"]
