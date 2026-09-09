FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends openssl && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
# By name, not `COPY . .`: the test file beside these two must not ship into an image with no
# pytest. Miss cpuwork.py here and the container exits at import with the host simply vanishing
# from telemetry -- which looks exactly like the collector-ordering trap and sends you to the
# wrong place.
COPY server.py cpuwork.py ./
ENV PORT=8000 ROLE=checkout PROFILE=healthy USE_TLS=0 CPU_SHARE=0
EXPOSE 8000
# gthread workers: handlers block on downstream calls + sleep, so give ample
# concurrency. USE_TLS=1 serves HTTPS with a self-signed cert (demo only) to
# test whether OBI's server-side capture (SSL uprobe path) avoids the plaintext
# HTTP/1 status=499 seen with context_propagation=all.
CMD ["sh", "-c", "\
  if [ \"$USE_TLS\" = \"1\" ]; then \
    openssl req -x509 -newkey rsa:2048 -nodes -keyout /tmp/k.pem -out /tmp/c.pem -days 3650 -subj '/CN=ping' >/dev/null 2>&1; \
    exec gunicorn -w 2 --threads 8 -b 0.0.0.0:${PORT} --certfile /tmp/c.pem --keyfile /tmp/k.pem --access-logfile - server:app; \
  else \
    exec gunicorn -w 2 --threads 8 -b 0.0.0.0:${PORT} --access-logfile - server:app; \
  fi"]
