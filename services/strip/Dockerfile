# services/strip — quote-stripping shared service (wasi-server-plan §11.1).
FROM python:3.11-slim

RUN groupadd --system strip && useradd --system --gid strip --create-home strip

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir --no-compile -r requirements.txt \
    # talon is installed separately with --no-deps — see requirements.txt's
    # header comment for why (it pulls an ML stack this service never
    # imports, plus a cchardet build that's broken on this Python version).
    && pip install --no-cache-dir --no-compile --no-deps talon==1.4.4

COPY app.py striplib.py ./

USER strip
EXPOSE 8080

# No --access-logfile: gunicorn only writes an access log if this flag is
# given, so omitting it is the actual control that keeps request bodies out
# of the container's logs (I-1) — not a log-format choice someone could
# quietly change later. See app.py's module docstring for the full I-1
# reasoning, including the app-level request log this still emits (path,
# status, duration — never body content).
CMD ["gunicorn", "--bind", "0.0.0.0:8080", "--workers", "2", "--timeout", "10", "app:app"]
