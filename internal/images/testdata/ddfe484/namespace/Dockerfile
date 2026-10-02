FROM {{.Base}}

RUN useradd -m -s /bin/bash {{.User}}
COPY provision.sh start.sh /tmp/cc-remote/
RUN bash /tmp/cc-remote/provision.sh packages \
    && bash /tmp/cc-remote/provision.sh tools \
    && install -m 0755 /tmp/cc-remote/start.sh /usr/local/bin/cc-remote-start \
    && rm -rf /tmp/cc-remote
{{- range .Layer}}
{{.}}
{{- end}}

USER {{.User}}
ENV PATH=/home/{{.User}}/.local/bin:${PATH}
