FROM mirror.gcr.io/ubuntu:noble

COPY server/server ./

CMD [ "/bin/sh", "-c", "./server" ]
