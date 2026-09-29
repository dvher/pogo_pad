FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/notes-server ./cmd/notes-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/notes-server /usr/local/bin/notes-server
ENV NOTES_DB=/data/notes.db NOTES_ADDR=:8080
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["notes-server"]
CMD ["serve"]
