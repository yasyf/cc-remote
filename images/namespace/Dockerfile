FROM {{.Base}}{{if .Layers}} AS system{{end}}

RUN useradd -m -s /bin/bash {{.User}}
COPY provision.sh start.sh plugins.sh finalize.sh baked.json /tmp/cc-remote/
RUN bash /tmp/cc-remote/provision.sh packages \
    && bash /tmp/cc-remote/provision.sh tools \
    && install -m 0755 /tmp/cc-remote/start.sh /usr/local/bin/cc-remote-start \
    && rm -f /etc/ssh/ssh_host_* \
    && bash /tmp/cc-remote/finalize.sh system /
{{- range .Layer}}
{{.}}
{{- end}}
{{- if .Layers}}

FROM system AS home
{{- end}}
RUN {{if .Private}}--mount=type=secret,id=github-token,required=true {{end}}install -d -o {{.User}} -g {{.User}} -m 0700 {{.Home}}/.cc-remote \
    && install -o {{.User}} -g {{.User}} -m 0600 /tmp/cc-remote/plugins.sh {{.Home}}/.cc-remote/plugins.sh \
    && { {{- if .Private}} cat /run/secrets/github-token;{{end}} echo; } | runuser -u {{.User}} -- env HOME={{.Home}} bash {{.Home}}/.cc-remote/plugins.sh install \
    && runuser -u {{.User}} -- env HOME={{.Home}} bash {{.Home}}/.cc-remote/plugins.sh natives \
    && bash /tmp/cc-remote/finalize.sh home {{.Home}} \
    && install -D -m 0644 /tmp/cc-remote/baked.json {{.Manifest}} \
    && rm -rf /tmp/cc-remote
{{- if .Layers}}

FROM home AS rest
RUN rm -rf{{range .Layers}} {{$.Home}}/{{.}}{{end}}

FROM system
RUN rm -rf /tmp/cc-remote
COPY --from=rest {{.Home}} {{.Home}}
COPY --from=rest {{.Manifest}} {{.Manifest}}
{{- range .Layers}}
COPY --from=home --chown={{$.User}}:{{$.User}} {{$.Home}}/{{.}} {{$.Home}}/{{.}}
{{- end}}
{{- end}}

USER {{.User}}
ENV PATH={{.Home}}/.local/bin:${PATH}
