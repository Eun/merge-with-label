FROM postgres:16-alpine

# Install pg_cron from Alpine's edge/testing repository.
RUN apk add --no-cache \
        --repository=https://dl-cdn.alpinelinux.org/alpine/edge/testing \
        pg_cron \
 # pg_cron must be loaded at startup and should run jobs in our database.
 && echo "shared_preload_libraries = 'pg_cron'" \
        >> /usr/local/share/postgresql/postgresql.conf.sample \
 && echo "cron.database_name = 'merge_with_label'" \
        >> /usr/local/share/postgresql/postgresql.conf.sample
