# Pinned by digest, not by tag. Measured on the fleet: no app host holds `python:3.12-slim` locally,
# so every per-host build pulls the base fresh -- and with three builds minutes apart from a moving
# tag, the three hosts could start from different bytes. The whole point of converting them one at a
# time is that the converted host differs from its peers ONLY by the change under test.
FROM python:3.12-slim@sha256:78387bc3881b8273120a12ebe6c1ab22b018ccc2c9adf565ae1ac9b536e184ea
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
