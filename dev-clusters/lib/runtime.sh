# Shared runtime primitives for the packaged development-cluster helpers.

is_running() {
  local pid="$1"
  [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null
}

cluster_running() {
  local slug="$1"
  local pid_path
  pid_path="$(pid_file "$slug")"
  [ -f "$pid_path" ] && runner_process_matches "$slug" "$(cat "$pid_path")"
}

runner_process_matches() {
  local slug="$1"
  local pid="$2"
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  is_running "$pid" || return 1
  process_has_argument "$pid" "$(socket_dir "$slug")"
}

process_has_argument() {
  local pid="$1"
  local expected="$2"
  local argument
  [ -r "/proc/$pid/cmdline" ] || return 1
  while IFS= read -r -d '' argument; do
    [ "$argument" = "$expected" ] && return 0
  done < "/proc/$pid/cmdline"
  return 1
}

socket_processes() {
  local slug="$1"
  local expected process pid
  expected="$(socket_dir "$slug")"
  for process in /proc/[0-9]*; do
    pid="${process##*/}"
    process_has_argument "$pid" "$expected" && printf '%s\n' "$pid"
  done
}

kill_socket_processes() {
  local slug="$1"
  local pid

  while IFS= read -r pid; do
    [ -n "$pid" ] || continue
    [ "$pid" = "$$" ] && continue
    kill -TERM "$pid" 2>/dev/null || true
  done < <(socket_processes "$slug")

  sleep 2

  while IFS= read -r pid; do
    [ -n "$pid" ] || continue
    [ "$pid" = "$$" ] && continue
    kill -KILL "$pid" 2>/dev/null || true
  done < <(socket_processes "$slug")
}

devcluster_read_small_file() {
  local path="$1"
  [ -e "$path" ] || [ -L "$path" ] || return 0
  [ ! -L "$path" ] && [ -f "$path" ] || die "unsafe cluster state file: $path"
  [ "$(stat -c %s "$path")" -le 4096 ] || die "cluster state file is too large: $path"
  cat "$path"
}

devcluster_status_json() {
  local kind="$1"
  local label="$2"
  local slug="$3"
  local ssh_command="$4"
  local include_services="$5"
  local directory topology network ready running state pid_text config_path credentials links

  directory="$(cluster_dir "$slug")"
  if [ ! -e "$directory" ] && [ ! -L "$directory" ]; then
    jq -n --arg kind "$kind" --arg label "$label" \
      '{schema: 1, found: false, kind: $kind, label: $label}'
    return
  fi
  [ ! -L "$directory" ] && [ -d "$directory" ] || die "unsafe cluster state directory"

  topology="$(devcluster_read_small_file "$(topology_file "$slug")")"
  network="$(devcluster_read_small_file "$(network_file "$slug")")"
  ready=false
  if [ -e "$(ready_file "$slug")" ]; then
    devcluster_read_small_file "$(ready_file "$slug")" >/dev/null
    ready=true
  fi
  running=false
  pid_text="$(devcluster_read_small_file "$(pid_file "$slug")")"
  if [ -n "$pid_text" ] && runner_process_matches "$slug" "$pid_text"; then
    running=true
  fi
  state=stopped
  [ "$running" = true ] && state=running
  [ "$ready" = true ] && [ "$running" = false ] && state=stale

  config_path="$directory/config.json"
  if [ -e "$config_path" ] || [ -L "$config_path" ]; then
    [ ! -L "$config_path" ] && [ -f "$config_path" ] || die "unsafe cluster config file"
    [ "$(stat -c %s "$config_path")" -le 1048576 ] || die "cluster config exceeds 1 MiB"
    jq -e . "$config_path" >/dev/null || die "invalid cluster config"
  else
    config_path=/dev/null
  fi

  credentials='[]'
  if declare -F devcluster_credentials_json >/dev/null; then
    credentials="$(devcluster_credentials_json "$slug")"
    jq -e 'type == "array"' <<<"$credentials" >/dev/null \
      || die "invalid development cluster credential catalog"
  fi
  links='[]'
  if [ "$kind" = vpsadmin ] && [ "$config_path" != /dev/null ]; then
    links="$(devcluster_vpsadmin_links_json "$config_path" "$network")"
  fi

  jq -n \
    --arg kind "$kind" \
    --arg label "$label" \
    --arg slug "$slug" \
    --arg state "$state" \
    --arg topology "$topology" \
    --arg network "$network" \
    --arg sshCommand "$ssh_command" \
    --argjson ready "$ready" \
    --argjson includeServices "$include_services" \
    --argjson credentials "$credentials" \
    --argjson links "$links" \
    --slurpfile config "$config_path" '
      ($config[0] // {}) as $cfg |
      ($cfg.topologies[$topology] // [] |
        if type == "array" then . else [] end) as $members |
      ([ $members[] |
        select(type == "string" and test("^[A-Za-z0-9][A-Za-z0-9_-]*$")) ]) as $machines |
      (if $includeServices then ["services"] + $machines else $machines end) as $targets |
      {
        schema: 1,
        found: true,
        kind: $kind,
        label: $label,
        state: $state,
        ready: $ready,
        topology: $topology,
        network: $network,
        links: $links,
        commands: ($targets | map({
          label: ., value: ($sshCommand + " ssh " + $slug + " " + .)
        })),
        credentials: $credentials
      }
    '
}

devcluster_vpsadmin_links_json() {
  local config="$1"
  local network="$2"

  jq -c --arg network "$network" '
    . as $cfg |
    [
      {key: "webui", label: "Web UI"},
      {key: "webCs", label: "Czech website"},
      {key: "webEn", label: "English website"},
      {key: "api", label: "API"},
      {key: "auth", label: "Authentication"},
      {key: "console", label: "Console"},
      {key: "mailpit", label: "Mailpit"},
      {key: "adminer", label: "Adminer"},
      {key: "status", label: "Status"}
    ] | map(
      . as $item |
      ($cfg.domains[$item.key] // "") as $domain |
      select($domain | type == "string" and length > 0) |
      {label: $item.label, url: (
        "https://" + $domain +
        (if $network == "local" then ":10443" else "" end) + "/"
      )}
    )
  ' "$config"
}
