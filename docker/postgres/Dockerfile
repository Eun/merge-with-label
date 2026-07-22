FROM postgres:16

# Install pg_cron from the official PostgreSQL apt repository.
# The pgdg apt source is already configured in the base image.
# pg_cron must be present on disk BEFORE the server starts, because
# shared_preload_libraries is evaluated at startup. Installing it here
# (image build time) is the only reliable way to satisfy that requirement.
RUN apt-get update \
 && apt-get install -y --no-install-recommends postgresql-16-cron \
 && rm -rf /var/lib/apt/lists/*
