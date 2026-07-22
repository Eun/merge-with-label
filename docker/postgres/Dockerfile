FROM postgres:16

# Install pg_cron from the PostgreSQL apt repository.
# pg_cron.so must be on disk before the server starts because
# shared_preload_libraries is evaluated at server startup.
# Installing it at image build time is the only reliable approach.
RUN apt-get update \
 && apt-get install -y --no-install-recommends postgresql-16-cron \
 && rm -rf /var/lib/apt/lists/*
