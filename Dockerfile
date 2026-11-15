# RAG 问答服务（rag/cmd/serve）的多阶段构建，系列第 7 篇《生产化》配套。
# 构建：docker build -t rag-serve .
# 运行：docker run -p 8080:8080 --env-file .env \
#         -v "$PWD/.vecdb:/data/.vecdb" -v "$PWD/.embed-cache:/data/.embed-cache" \
#         rag-serve -db /data/.vecdb -cache /data/.embed-cache

# ---- 构建阶段：官方 Go 镜像，版本与 go.mod 的 go 指令对齐 ----
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 全链路纯 Go（chromem-go / modernc.org/sqlite 均无 CGO），静态编译即可
RUN CGO_ENABLED=0 go build -trimpath -o /out/ragserve ./rag/cmd/serve

# ---- 运行阶段：极简 alpine，只带一个静态二进制 ----
FROM alpine:3
COPY --from=build /out/ragserve /usr/local/bin/ragserve
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["ragserve"]
