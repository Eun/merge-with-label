FROM postgres:16

# Install pg_cron from the official PostgreSQL apt repository.
# The pgdg apt source is already configured in the base image.
RUN apt-get update \
 && apt-get install -y --no-install-recommends postgresql-16-cron \
 && rm -rf /var/lib/apt/lists/*

# pg_cron must be listed in shared_preload_libraries and should run
# jobs in the application database (not the default 'postgres' db).
RUN echo "shared_preload_libraries = 'pg_cron'" \
        >> /usr/share/postgresql/postgresql.conf.sample \
 && echo "cron.database_name = 'merge_with_label'" \
        >> /usr/share/postgresql/postgresql.conf.sample
