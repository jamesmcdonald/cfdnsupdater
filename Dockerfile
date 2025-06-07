FROM cgr.dev/chainguard/glibc-dynamic

WORKDIR /app
COPY cfdnsupdater .

EXPOSE 9876
ENTRYPOINT ["./cfdnsupdater"]
