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

process_references_path() {
  local pid="$1"
  local expected="$2"
  local argument
  [ -r "/proc/$pid/cmdline" ] || return 1
  while IFS= read -r -d '' argument; do
    case "$argument" in
      "$expected"|"$expected"/*|*="$expected"|*="$expected"/*|*,"$expected"|*,"$expected"/*)
        return 0
        ;;
    esac
  done < "/proc/$pid/cmdline"
  return 1
}

socket_processes() {
  local slug="$1"
  local expected process pid
  expected="$(socket_dir "$slug")"
  for process in /proc/[0-9]*; do
    pid="${process##*/}"
    process_references_path "$pid" "$expected" && printf '%s\n' "$pid"
  done
}

kill_socket_processes() {
  local slug="$1"
  local expected pid
  expected="$(socket_dir "$slug")"

  while IFS= read -r pid; do
    [ -n "$pid" ] || continue
    [ "$pid" = "$$" ] && continue
    process_references_path "$pid" "$expected" || continue
    kill -TERM "$pid" 2>/dev/null || true
  done < <(socket_processes "$slug")

  sleep 2

  while IFS= read -r pid; do
    [ -n "$pid" ] || continue
    [ "$pid" = "$$" ] && continue
    process_references_path "$pid" "$expected" || continue
    kill -KILL "$pid" 2>/dev/null || true
  done < <(socket_processes "$slug")
}

devcluster_check_directory() {
  local path="$1"
  local label="$2"
  local create="${3:-false}"

  [ ! -L "$path" ] || die "unsafe $label symlink: $path"
  if [ -e "$path" ]; then
    [ -d "$path" ] || die "unsafe $label: $path"
    return 0
  fi
  [ "$create" = true ] || return 1
  mkdir -- "$path"
  [ ! -L "$path" ] && [ -d "$path" ] || die "unable to create safe $label: $path"
}

devcluster_check_regular_file() {
  local path="$1"
  local label="$2"

  [ -e "$path" ] || [ -L "$path" ] || return 1
  [ ! -L "$path" ] && [ -f "$path" ] || die "unsafe $label: $path"
}

devcluster_validate_ssh_directory() {
  local create="${1:-false}"

  if ! devcluster_validate_provider_root "$create"; then
    return 1
  fi
  if ! devcluster_check_directory "$SSH_DIR" \
    "$DEVCLUSTER_KIND SSH state directory" "$create"; then
    return 1
  fi
  devcluster_check_regular_file "$SSH_KEY" \
    "$DEVCLUSTER_KIND SSH private key" || true
  devcluster_check_regular_file "$SSH_KEY.pub" \
    "$DEVCLUSTER_KIND SSH public key" || true
}

devcluster_validate_provider_root() {
  local create="${1:-false}"
  local base="$WORKSPACE/.dev-clusters"

  if ! devcluster_check_directory "$base" "development cluster state root" "$create"; then
    return 1
  fi
  devcluster_check_directory "$base/$DEVCLUSTER_KIND" \
    "$DEVCLUSTER_KIND state root" "$create"
}

devcluster_validate_state_ancestors() {
  local create="${1:-false}"

  if ! devcluster_validate_provider_root "$create"; then
    return 1
  fi
  devcluster_check_directory "$STATE_ROOT/clusters" \
    "$DEVCLUSTER_KIND cluster directory" "$create"
}

devcluster_validate_cluster_directory() {
  local slug="$1"
  local create="${2:-false}"
  local directory

  if ! devcluster_validate_state_ancestors "$create"; then
    return 1
  fi
  directory="$(cluster_dir "$slug")"
  devcluster_check_directory "$directory" \
    "$DEVCLUSTER_KIND cluster state directory" "$create"
}

devcluster_require_active_session() {
  local slug="$1"
  local work_root="$WORKSPACE/work"
  local archive_root="$WORKSPACE/archive"
  local directory="$work_root/$slug"
  local state="$directory/state.md"
  local -a header=()

  devcluster_check_directory "$work_root" "workspace work root" false \
    || die "development session '$slug' is not active"
  devcluster_check_directory "$directory" "development session directory" false \
    || die "development session '$slug' is not active"
  [ ! -L "$state" ] && [ -f "$state" ] \
    || die "development session '$slug' has unsafe or missing state.md"
  [ "$(stat -c %s "$state")" -le 1048576 ] \
    || die "development session '$slug' state.md exceeds 1 MiB"
  mapfile -t -n 3 header < "$state"
  header[0]="${header[0]-}"
  header[0]="${header[0]%$'\r'}"
  header[1]="${header[1]-}"
  header[1]="${header[1]%$'\r'}"
  header[2]="${header[2]-}"
  header[2]="${header[2]%$'\r'}"
  [ "${header[0]-}" = "---" ] && [ "${header[1]-}" = "lifecycle: active" ] \
    && [ "${header[2]-}" = "---" ] \
    || die "development session '$slug' is not active"

  if [ -e "$archive_root" ] || [ -L "$archive_root" ]; then
    devcluster_check_directory "$archive_root" "workspace archive root" false
  fi
  if [ -e "$archive_root/$slug" ] || [ -L "$archive_root/$slug" ]; then
    die "development session '$slug' also exists in the archive"
  fi
}

devcluster_lock_root() {
  local base="$WORKSPACE/.dev-clusters"
  local root="$base/.locks"

  devcluster_check_directory "$base" "development cluster state root" true
  devcluster_check_directory "$root" "development cluster lock directory" true
  [ "$(stat -c %u "$root")" = "$(id -u)" ] \
    || die "development cluster lock directory is not owned by the current uid: $root"
  chmod 700 "$root"
  printf '%s\n' "$root"
}

devcluster_exec_without_lifecycle_lock() {
  local inherited_fd="${DEVCLUSTER_LIFECYCLE_LOCK_FD:-}"

  [[ "$inherited_fd" =~ ^[0-9]+$ ]] \
    || die "development cluster lifecycle lock is unavailable"
  exec {inherited_fd}>&-
  unset DEVCLUSTER_LIFECYCLE_LOCK_FD
  exec "$@"
}

devcluster_with_lock() {
  local name="$1"
  local callback="$2"
  shift 2
  local root path lock_fd path_identity fd_identity result
  local inherited_lock_fd="${DEVCLUSTER_LIFECYCLE_LOCK_FD:-}"

  [[ "$name" =~ ^[A-Za-z0-9][A-Za-z0-9_-]*$ ]] \
    || die "invalid development cluster lock name '$name'"

  root="$(devcluster_lock_root)"
  path="$root/$name.lock"
  [ ! -L "$path" ] || die "unsafe development cluster lock symlink: $path"
  if [ ! -e "$path" ]; then
    if ! (set -o noclobber; umask 077; : > "$path") 2>/dev/null; then
      [ -e "$path" ] || die "unable to create development cluster lock: $path"
    fi
  fi
  [ ! -L "$path" ] && [ -f "$path" ] \
    || die "unsafe development cluster lock: $path"
  [ "$(stat -c %u "$path")" = "$(id -u)" ] && [ "$(stat -c %a "$path")" = 600 ] \
    || die "development cluster lock must be an owned mode-0600 file: $path"

  exec {lock_fd}<> "$path"
  path_identity="$(stat -c '%d:%i' "$path")"
  fd_identity="$(stat -Lc '%d:%i' "/proc/$$/fd/$lock_fd")"
  [ "$path_identity" = "$fd_identity" ] \
    || die "development cluster lock changed while opening: $path"
  flock -x "$lock_fd"

  DEVCLUSTER_LIFECYCLE_LOCK_FD="$lock_fd"
  if "$callback" "$@"; then
    result=0
  else
    result=$?
  fi
  if [ -n "$inherited_lock_fd" ]; then
    DEVCLUSTER_LIFECYCLE_LOCK_FD="$inherited_lock_fd"
  else
    unset DEVCLUSTER_LIFECYCLE_LOCK_FD
  fi
  flock -u "$lock_fd"
  exec {lock_fd}>&-
  return "$result"
}

devcluster_lifecycle_callback() {
  local slug="$1"
  local require_active="$2"
  local callback="$3"
  shift 3

  devcluster_validate_cluster_directory "$slug" false || true
  if [ "$require_active" = true ]; then
    devcluster_require_active_session "$slug"
  fi
  "$callback" "$slug" "$@"
}

devcluster_with_lifecycle_lock() {
  local slug="$1"
  local require_active="$2"
  local callback="$3"
  shift 3

  devcluster_with_lock "$DEVCLUSTER_KIND-$slug" \
    devcluster_lifecycle_callback "$slug" "$require_active" "$callback" "$@"
}

list_cluster_slugs() {
  local dir
  [ -d "$STATE_ROOT/clusters" ] || return 0

  for dir in "$STATE_ROOT"/clusters/*; do
    [ ! -L "$dir" ] && [ -d "$dir" ] || continue
    basename "$dir"
  done | sort
}

remove_result_link() {
  local slug="$1"
  local link

  devcluster_validate_cluster_directory "$slug" false || return 0
  link="$(result_link "$slug")"

  if [ -L "$link" ] || [ -e "$link" ]; then
    rm -f -- "$link"
    printf 'removed GC root: %s\n' "$link"
  fi
}

gcroots_cluster() {
  local cleanup=0
  local -a slugs=()

  devcluster_validate_state_ancestors false || true

  while [ $# -gt 0 ]; do
    case "$1" in
      --cleanup)
        cleanup=1
        ;;
      --*)
        usage
        exit 2
        ;;
      *)
        slugs+=("$1")
        ;;
    esac
    shift
  done

  if [ "${#slugs[@]}" -eq 0 ]; then
    mapfile -t slugs < <(list_cluster_slugs)
  fi

  local slug
  for slug in "${slugs[@]}"; do
    [[ "$slug" =~ ^[A-Za-z0-9][A-Za-z0-9_-]*$ ]] || die "invalid cluster slug '$slug'"
    if [ "$cleanup" = 1 ]; then
      devcluster_with_lifecycle_lock "$slug" false gcroot_cluster "$cleanup"
    else
      gcroot_cluster "$slug" "$cleanup"
    fi
  done
}

gcroot_cluster() {
  local slug="$1"
  local cleanup="$2"
  local link target state

  devcluster_validate_cluster_directory "$slug" false || return 0
  link="$(result_link "$slug")"

  if cluster_running "$slug"; then
    state="running"
  elif [ -f "$(ready_file "$slug")" ]; then
    state="stopped-stale-ready"
  else
    state="stopped"
  fi

  if [ ! -L "$link" ] && [ ! -e "$link" ]; then
    printf '%s %s no-gcroot\n' "$slug" "$state"
    return
  fi

  target="$(readlink -f "$link" 2>/dev/null || true)"

  if [ "$cleanup" = "1" ] && [ "$state" != "running" ]; then
    remove_result_link "$slug"
  else
    printf '%s %s %s' "$slug" "$state" "$link"
    [ -n "$target" ] && printf ' -> %s' "$target"
    printf '\n'
  fi
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
  local slug="$2"
  local ssh_command="$3"
  local include_services="$4"
  local directory topology network ready running state pid_text config_path credentials links

  devcluster_validate_state_ancestors false || true
  directory="$(cluster_dir "$slug")"
  if [ ! -e "$directory" ] && [ ! -L "$directory" ]; then
    jq -n --arg kind "$kind" '{schema: 1, found: false, kind: $kind}'
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
