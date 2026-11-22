FROM python:3.12-alpine

WORKDIR /app

# Sin dependencias externas: solo stdlib.
COPY api/app.py /app/app.py
COPY public /app/public

RUN adduser -D -u 10001 apiuser && \
    mkdir -p /data && chown -R apiuser:apiuser /data /app

USER apiuser

ENV PYTHONUNBUFFERED=1 \
    PORT=8080 \
    SEARX_URL=http://searxng:8080 \
    CACHE_DB=/data/cache.db \
    STATIC_DIR=/app/public

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:8080/health',timeout=4).status==200 else 1)"

CMD ["python", "/app/app.py"]
