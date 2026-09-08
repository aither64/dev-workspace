# Shared runtime primitives for the packaged development-cluster helpers.

devcluster_load_runtime_contract() {
  [ "${DEVCLUSTER_RUNTIME_CONTRACT_LOADED:-}" = 1 ] && return

  local runtime_directory contract candidate tracking_max journal_rows
  runtime_directory="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
  contract=""
  for candidate in \
    "$runtime_directory/../../portal/internal/session/runtime-contract.json" \
    "$runtime_directory/../runtime-contract.json"; do
    [ -n "$candidate" ] || continue
    if [ -f "$candidate" ]; then
      contract="$candidate"
      break
    fi
  done
  [ -n "$contract" ] || die "workspace runtime contract is missing"
  [ ! -L "$contract" ] && [ -f "$contract" ] \
    || die "workspace runtime contract is unsafe: $contract"
  need_cmd jq

  tracking_max="$(jq -er '
    .trackingMaxBytes |
    select(type == "number" and . == floor and . > 0)
  ' "$contract")" || die "workspace runtime contract has an invalid tracking limit"
  journal_rows="$(jq -er '
    .lifecycleJournals |
    select(
      type == "array" and length > 0 and
      all(.[];
        type == "object" and
        (keys | sort) == ["command", "name"] and
        (.name | type == "string" and test("^[A-Za-z0-9][A-Za-z0-9_-]*$")) and
        (.command == "archive" or .command == "delete" or .command == "revive")
      ) and
      (group_by(.name) | all(.[]; length == 1)) and
      (group_by(.command) | all(.[]; length == 1)) and
      ([.[].command] | sort == ["archive", "delete", "revive"])
    ) |
    .[] | [.name, .command] | @tsv
  ' "$contract")" || die "workspace runtime contract has invalid lifecycle journals"

  DEVCLUSTER_TRACKING_MAX_BYTES="$tracking_max"
  mapfile -t DEVCLUSTER_LIFECYCLE_JOURNALS <<< "$journal_rows"
  DEVCLUSTER_RUNTIME_CONTRACT_LOADED=1
}

is_running() {
  local pid="$1"
  [ -n "$pid" ] && [ -d "/proc/$pid" ]
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

signal_cluster_runner() {
  local slug="$1"
  local pid="$2"
  local signal="$3"
  local action="$4"

  runner_process_matches "$slug" "$pid" || return 0
  if ! kill "-$signal" "$pid" 2>/dev/null && runner_process_matches "$slug" "$pid"; then
    die "unable to $action cluster runner PID $pid"
  fi
}

signal_cluster_runner_socket() {
  local slug="$1"
  local pid="$2"
  local runner_socket="$3"
  local signal="$4"
  local action="$5"

  runner_process_matches_socket "$slug" "$pid" "$runner_socket" || return 0
  if ! kill "-$signal" "$pid" 2>/dev/null &&
    runner_process_matches_socket "$slug" "$pid" "$runner_socket"; then
    die "unable to $action cluster runner PID $pid"
  fi
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

process_has_argument_pair() {
  local pid="$1"
  local expected_option="$2"
  local expected_value="$3"
  local argument previous=""
  [ -r "/proc/$pid/cmdline" ] || return 1
  while IFS= read -r -d '' argument; do
    if [ "$previous" = "$expected_option" ] && [ "$argument" = "$expected_value" ]; then
      return 0
    fi
    previous="$argument"
  done < "/proc/$pid/cmdline"
  return 1
}

process_is_descendant_of() {
  local pid="$1"
  local ancestor="$2"
  local key value
  local depth=0
  [[ "$pid" =~ ^[0-9]+$ ]] && [[ "$ancestor" =~ ^[0-9]+$ ]] || return 1

  while [ "$depth" -lt 64 ] && [ "$pid" -gt 1 ]; do
    [ "$pid" = "$ancestor" ] && return 0
    value=""
    while read -r key value; do
      [ "$key" = "PPid:" ] && break
    done < "/proc/$pid/status" 2>/dev/null || return 1
    [[ "$value" =~ ^[0-9]+$ ]] || return 1
    pid="$value"
    depth=$((depth + 1))
  done
  return 1
}

runner_process_matches_socket() {
  local slug="$1"
  local pid="$2"
  local runner_socket="$3"
  local directory
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  is_running "$pid" || return 1
  directory="$(cluster_dir "$slug")"
  process_has_argument_pair "$pid" --sock-dir "$runner_socket" &&
    process_has_argument_pair "$pid" --state-dir "$directory/state" &&
    process_has_argument_pair "$pid" --pid-file "$(pid_file "$slug")" &&
    process_has_argument_pair "$pid" --ready-file "$(ready_file "$slug")"
}

legacy_runner_process_matches() {
  runner_process_matches_socket "$@"
}

legacy_socket_transition_allowed() {
  local slug="$1"
  local prefix="$2"

  [ "$slug" = "2026-08-18-vpsadmin-password-reset" ] &&
    [ "$prefix" = "vpsfree-devcluster" ]
}

legacy_socket_owner_file() {
  printf '%s/legacy-socket-owner\n' "$(cluster_dir "$1")"
}

legacy_socket_owner_identity() {
  printf '%s\0%s' "$WORKSPACE" "$1" | sha256sum | cut -c1-64
}

record_legacy_socket_owner() {
  local slug="$1"
  local socket_path="$2"
  local path expected temporary recorded
  path="$(legacy_socket_owner_file "$slug")"
  expected="$(legacy_socket_owner_identity "$socket_path")"

  if [ -e "$path" ] || [ -L "$path" ]; then
    [ ! -L "$path" ] && [ -f "$path" ] \
      || die "unsafe legacy cluster socket owner: $path"
    recorded="$(cat "$path")"
    [ "$recorded" = "$expected" ] \
      || die "legacy cluster socket belongs to another workspace: $socket_path"
    return
  fi

  temporary="${path}.$$"
  [ ! -e "$temporary" ] && [ ! -L "$temporary" ] \
    || die "temporary legacy cluster socket owner exists: $temporary"
  (umask 077; printf '%s\n' "$expected" > "$temporary")
  mv -- "$temporary" "$path"
}

legacy_socket_owner_matches() {
  local slug="$1"
  local socket_path="$2"
  local path expected recorded
  path="$(legacy_socket_owner_file "$slug")"
  [ -e "$path" ] || [ -L "$path" ] || return 1
  [ ! -L "$path" ] && [ -f "$path" ] \
    || die "unsafe legacy cluster socket owner: $path"
  expected="$(legacy_socket_owner_identity "$socket_path")"
  recorded="$(cat "$path")"
  [ "$recorded" = "$expected" ]
}

devcluster_workspace_socket_dir() {
  local slug="$1"
  local prefix="$2"
  local digest
  digest="$(printf '%s\0%s' "$WORKSPACE" "$slug" | sha256sum | cut -c1-12)"
  printf '/tmp/%s-%s\n' "$prefix" "$digest"
}

devcluster_legacy_socket_dir() {
  local slug="$1"
  local prefix="$2"
  local digest
  digest="$(printf '%s' "$slug" | sha256sum | cut -c1-12)"
  printf '/tmp/%s-%s\n' "$prefix" "$digest"
}

record_cluster_socket_dir() {
  local slug="$1"
  local selected="$2"
  local state_file temporary recorded
  [ -d "$(cluster_dir "$slug")" ] || return 0
  state_file="$(cluster_dir "$slug")/socket-dir"
  if [ -e "$state_file" ] || [ -L "$state_file" ]; then
    [ ! -L "$state_file" ] && [ -f "$state_file" ] \
      || die "unsafe cluster socket state: $state_file"
    recorded="$(cat "$state_file")"
    [ "$recorded" = "$selected" ] && return 0
    die "refusing to replace different cluster socket state: $state_file"
  fi
  temporary="${state_file}.$$"
  [ ! -e "$temporary" ] && [ ! -L "$temporary" ] \
    || die "temporary cluster socket state exists: $temporary"
  (umask 077; printf '%s\n' "$selected" > "$temporary")
  mv -- "$temporary" "$state_file"
}

devcluster_initialize_cluster_socket_identity() {
  local slug="$1"
  local prefix="$2"
  local directory selected
  directory="$(cluster_dir "$slug")"

  if [ -e "$directory" ] || [ -L "$directory" ]; then
    devcluster_validate_cluster_directory "$slug" false \
      || die "development cluster state disappeared while initializing: $slug"
    devcluster_socket_dir "$slug" "$prefix" >/dev/null
    return
  fi

  # This is the only transaction that turns an absent cluster into new state.
  # It runs under the per-cluster lifecycle lock, so a stale packaged helper
  # that creates pre-contract state first makes the existing-state branch above
  # fail closed instead of being silently relabeled.
  devcluster_validate_cluster_directory "$slug" true
  selected="$(devcluster_workspace_socket_dir "$slug" "$prefix")"
  record_cluster_socket_dir "$slug" "$selected"
}

devcluster_adopt_package_transition() {
  local slug="$1"
  local prefix="$2"
  local directory state_file canonical legacy recorded pid
  directory="$(cluster_dir "$slug")"
  [ -e "$directory" ] || [ -L "$directory" ] \
    || die "development cluster state disappeared during package transition: $slug"
  [ ! -L "$directory" ] && [ -d "$directory" ] \
    || die "unsafe development cluster state during package transition: $directory"

  canonical="$(devcluster_workspace_socket_dir "$slug" "$prefix")"
  legacy="$(devcluster_legacy_socket_dir "$slug" "$prefix")"
  state_file="$directory/socket-dir"
  if [ -e "$state_file" ] || [ -L "$state_file" ]; then
    [ ! -L "$state_file" ] && [ -f "$state_file" ] \
      || die "unsafe cluster socket state: $state_file"
    recorded="$(cat "$state_file")"
    if [ "$recorded" = "$canonical" ]; then
      local -a legacy_pids=()
      if [ -e "$legacy" ] || [ -L "$legacy" ]; then
        [ ! -L "$legacy" ] && [ -d "$legacy" ] \
          || die "unsafe reappeared legacy cluster socket directory: $legacy"
        die "legacy cluster socket directory reappeared after package adoption: $legacy"
      fi
      mapfile -t legacy_pids < <(socket_processes "$slug" "$legacy")
      if [ "${#legacy_pids[@]}" -gt 0 ]; then
        die "legacy cluster process appeared after package adoption: ${legacy_pids[*]}"
      fi
      if [ -f "$(pid_file "$slug")" ]; then
        pid="$(cat "$(pid_file "$slug")")"
        legacy_runner_process_matches "$slug" "$pid" "$legacy" &&
          die "legacy cluster runner appeared after package adoption: $pid"
      fi
      return 0
    fi
    [ "$recorded" = "$legacy" ] &&
      legacy_socket_transition_allowed "$slug" "$prefix" \
      || die "cluster state cannot be adopted by this package: $slug"
  else
    die "pre-contract cluster state has no recorded socket identity: $slug; reset this cluster first"
  fi

  if legacy_socket_transition_allowed "$slug" "$prefix" &&
    [ -f "$(pid_file "$slug")" ]; then
    pid="$(cat "$(pid_file "$slug")")"
    if legacy_runner_process_matches "$slug" "$pid" "$recorded"; then
      record_legacy_socket_owner "$slug" "$recorded"
      record_cluster_socket_dir "$slug" "$recorded"
      return 0
    fi
  fi
  if legacy_socket_transition_allowed "$slug" "$prefix" &&
    legacy_socket_owner_matches "$slug" "$recorded" &&
    { [ -e "$recorded" ] || [ -L "$recorded" ]; }; then
    [ ! -L "$recorded" ] && [ -d "$recorded" ] \
      || die "unsafe legacy cluster socket directory: $recorded"
    record_cluster_socket_dir "$slug" "$recorded"
    return 0
  fi
  die "legacy cluster ownership cannot be proven during package transition: $slug"
}

devcluster_cleanup_paths_json() {
  local slug="$1"
  local prefix="$2"
  local state_path socket_path legacy_path=""
  state_path="$(cluster_dir "$slug")"
  socket_path="$(devcluster_workspace_socket_dir "$slug" "$prefix")"
  if legacy_socket_transition_allowed "$slug" "$prefix"; then
    legacy_path="$(devcluster_legacy_socket_dir "$slug" "$prefix")"
  fi
  jq -cn \
    --arg state "$state_path" \
    --arg socket "$socket_path" \
    --arg legacy "$legacy_path" \
    '{schema: 1, paths: ([$state, $socket] + (if $legacy == "" then [] else [$legacy] end))}'
}

devcluster_socket_dir() {
  local slug="$1"
  local prefix="$2"
  local legacy selected state_file pid recorded
  legacy="$(devcluster_legacy_socket_dir "$slug" "$prefix")"
  selected="$(devcluster_workspace_socket_dir "$slug" "$prefix")"
  state_file="$(cluster_dir "$slug")/socket-dir"

  if [ -e "$state_file" ] || [ -L "$state_file" ]; then
    [ ! -L "$state_file" ] && [ -f "$state_file" ] \
      || die "unsafe cluster socket state: $state_file"
    recorded="$(cat "$state_file")"
    case "$recorded" in
      "$selected")
        printf '%s\n' "$selected"
        return
        ;;
      "$legacy")
        if legacy_socket_transition_allowed "$slug" "$prefix"; then
          if [ -f "$(pid_file "$slug")" ]; then
            pid="$(cat "$(pid_file "$slug")")"
            if legacy_runner_process_matches "$slug" "$pid" "$recorded"; then
              record_legacy_socket_owner "$slug" "$recorded"
              printf '%s\n' "$recorded"
              return
            fi
          fi
          if legacy_socket_owner_matches "$slug" "$recorded"; then
            if [ -e "$recorded" ] || [ -L "$recorded" ]; then
              [ ! -L "$recorded" ] && [ -d "$recorded" ] \
                || die "unsafe legacy cluster socket directory: $recorded"
            fi
            printf '%s\n' "$recorded"
            return
          fi
        fi
        die "legacy cluster ownership cannot be proven: $slug; reset this cluster first"
        ;;
      *) die "invalid cluster socket state: $state_file" ;;
    esac
  fi

  die "cluster socket identity is missing: $slug; reset this cluster first"
}

devcluster_reset_socket_dir() {
  local slug="$1"
  local prefix="$2"
  local directory state_file canonical legacy recorded pid candidate process
  local -a referenced=()
  directory="$(cluster_dir "$slug")"
  canonical="$(devcluster_workspace_socket_dir "$slug" "$prefix")"
  legacy="$(devcluster_legacy_socket_dir "$slug" "$prefix")"
  state_file="$directory/socket-dir"

  recorded=""
  if [ -e "$state_file" ] || [ -L "$state_file" ]; then
    [ ! -L "$state_file" ] && [ -f "$state_file" ] \
      || die "unsafe cluster socket state: $state_file"
    recorded="$(cat "$state_file")"
    case "$recorded" in
      "$canonical") printf '%s\n' "$recorded"; return ;;
      "$legacy")
        if legacy_socket_transition_allowed "$slug" "$prefix" &&
          legacy_socket_owner_matches "$slug" "$legacy"; then
          printf '%s\n' "$legacy"
          return
        fi
        ;;
      *) die "invalid cluster socket state: $state_file" ;;
    esac
  elif legacy_socket_transition_allowed "$slug" "$prefix" &&
    legacy_socket_owner_matches "$slug" "$legacy"; then
    [ -e "$legacy" ] || [ -L "$legacy" ] \
      || die "recorded legacy cluster socket is missing: $legacy"
    [ ! -L "$legacy" ] && [ -d "$legacy" ] \
      || die "unsafe legacy cluster socket directory: $legacy"
    printf '%s\n' "$legacy"
    return
  fi

  pid=""
  if [ -f "$(pid_file "$slug")" ]; then
    pid="$(cat "$(pid_file "$slug")")"
  fi
  for candidate in "$canonical" "$legacy" "$directory"; do
    while IFS= read -r process; do
      [ -n "$process" ] || continue
      [[ " ${referenced[*]-} " = *" $process "* ]] || referenced+=("$process")
    done < <(socket_processes "$slug" "$candidate")
  done

  if [ -z "$recorded" ] && [ -n "$pid" ] &&
    runner_process_matches_socket "$slug" "$pid" "$canonical"; then
    candidate="$canonical"
  elif [ -n "$pid" ] && runner_process_matches_socket "$slug" "$pid" "$legacy"; then
    candidate="$legacy"
  else
    candidate="$canonical"
    if [ "${#referenced[@]}" -gt 0 ]; then
      die "cluster socket identity is missing and process ownership cannot be proven: $slug"
    fi
    if [ -e "$legacy" ] || [ -L "$legacy" ]; then
      die "cluster socket identity is missing and the legacy socket is ambiguous: $slug"
    fi
    [ -z "$recorded" ] || candidate="$recorded"
  fi

  for process in "${referenced[@]}"; do
    { [ "$process" = "$pid" ] || process_is_descendant_of "$process" "$pid"; } \
      || die "cluster socket identity is missing and process ownership cannot be proven: $slug"
  done
  printf '%s\n' "$candidate"
}

devcluster_reset_cluster_runtime() {
  local slug="$1"
  local prefix="$2"
  local sock_dir legacy pid
  local -a processes=()

  if ! sock_dir="$(devcluster_reset_socket_dir "$slug" "$prefix")"; then
    return 1
  fi
  legacy="$(devcluster_legacy_socket_dir "$slug" "$prefix")"

  if [ -f "$(pid_file "$slug")" ]; then
    pid="$(cat "$(pid_file "$slug")")"
    if runner_process_matches_socket "$slug" "$pid" "$sock_dir"; then
      signal_cluster_runner_socket "$slug" "$pid" "$sock_dir" TERM stop
      for _ in $(seq 1 120); do
        runner_process_matches_socket "$slug" "$pid" "$sock_dir" || break
        sleep 1
      done
      if runner_process_matches_socket "$slug" "$pid" "$sock_dir"; then
        signal_cluster_runner_socket "$slug" "$pid" "$sock_dir" KILL kill
      fi
    fi
  fi

  if [ "$sock_dir" = "$legacy" ]; then
    mapfile -t processes < <(socket_processes "$slug" "$sock_dir")
    [ "${#processes[@]}" -eq 0 ] \
      || die "legacy cluster processes remain after stopping the proven runner: ${processes[*]}"
  else
    kill_socket_processes "$slug" "$sock_dir"
  fi
  remove_result_link "$slug"
  remove_cluster_runtime_state "$slug" "$sock_dir"
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
  local requested="${2:-}"
  local expected process pid
  expected="${requested:-$(socket_dir "$slug")}"
  for process in /proc/[0-9]*; do
    pid="${process##*/}"
    [ "$pid" = "$$" ] && continue
    process_references_path "$pid" "$expected" && printf '%s\n' "$pid"
  done
}

kill_socket_processes() {
  local slug="$1"
  local requested="${2:-}"
  local expected pid
  local -a processes=()
  expected="${requested:-$(socket_dir "$slug")}"

  mapfile -t processes < <(socket_processes "$slug" "$expected")
  [ "${#processes[@]}" -gt 0 ] || return 0
  for pid in "${processes[@]}"; do
    process_references_path "$pid" "$expected" || continue
    kill -TERM "$pid" 2>/dev/null || true
  done

  sleep 2

  mapfile -t processes < <(socket_processes "$slug" "$expected")
  for pid in "${processes[@]}"; do
    process_references_path "$pid" "$expected" || continue
    kill -KILL "$pid" 2>/dev/null || true
  done

  for _ in $(seq 1 20); do
    mapfile -t processes < <(socket_processes "$slug" "$expected")
    [ "${#processes[@]}" -gt 0 ] || return 0
    sleep 0.1
  done

  mapfile -t processes < <(socket_processes "$slug" "$expected")
  [ "${#processes[@]}" -gt 0 ] || return 0
  die "unable to stop cluster processes: ${processes[*]}"
}

remove_cluster_runtime_state() {
  local slug="$1"
  local socket_path="$2"

  # The socket selection for a legacy cluster is recorded inside cluster_dir.
  # Remove the captured path before destroying that record so an interrupted
  # reset can retry without silently switching to the workspace-scoped path.
  rm -rf -- "$socket_path"
  rm -rf -- "$(cluster_dir "$slug")"
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
  mkdir -- "$path" 2>/dev/null || true
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

  devcluster_load_runtime_contract

  devcluster_check_directory "$work_root" "workspace work root" false \
    || die "development session '$slug' is not active"
  devcluster_check_directory "$directory" "development session directory" false \
    || die "development session '$slug' is not active"
  [ ! -L "$state" ] && [ -f "$state" ] \
    || die "development session '$slug' has unsafe or missing state.md"
  [ "$(stat -c %s "$state")" -le "$DEVCLUSTER_TRACKING_MAX_BYTES" ] \
    || die "development session '$slug' state.md exceeds the shared tracking limit"
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

devcluster_require_lifecycle_lock_owner() {
  local slug="$1"
  local owner="$2"
  local lock_fd="${VPSFREE_DEV_SESSION_LIFECYCLE_LOCK_FD:-}"
  local lock_path="${VPSFREE_DEV_SESSION_LIFECYCLE_LOCK_PATH:-}"
  local workspace_name="${VPSFREE_WORKSPACE_NAME:-}"
  local runtime_root
  local fallback_path="$WORKSPACE/worktrees/.locks/$slug.lock"
  local authority_path=""
  local lock_identity fd_identity probe_fd

  [[ "$lock_fd" =~ ^[3-9][0-9]*$ ]] \
    || die "development session lifecycle lock is unavailable"
  [ -n "$lock_path" ] && [ "${lock_path:0:1}" = / ] \
    || die "development session lifecycle lock path is invalid"
  if [ -n "${VPSFREE_WORKSPACES_RUNTIME_DIR:-}" ]; then
    runtime_root="$VPSFREE_WORKSPACES_RUNTIME_DIR"
  else
    runtime_root="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/vpsfree-workspaces"
  fi
  if [[ "$workspace_name" =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]]; then
    authority_path="$runtime_root/$workspace_name/authority/$slug.lock"
  fi
  if [ "$lock_path" != "$fallback_path" ] \
    && { [ -z "$authority_path" ] || [ "$lock_path" != "$authority_path" ]; }; then
    die "development session lifecycle lock does not belong to '$slug'"
  fi
  [ ! -L "$lock_path" ] && [ -f "$lock_path" ] \
    || die "development session lifecycle lock is unsafe"
  [ "$(stat -c %u "$lock_path")" = "$(id -u)" ] \
    && [ "$(stat -c %a "$lock_path")" = 600 ] \
    || die "development session lifecycle lock must be an owned mode-0600 file"
  [ -e "/proc/$$/fd/$lock_fd" ] \
    || die "development session lifecycle lock descriptor is unavailable"
  lock_identity="$(stat -c '%d:%i' "$lock_path")"
  fd_identity="$(stat -Lc '%d:%i' "/proc/$$/fd/$lock_fd")"
  [ "$lock_identity" = "$fd_identity" ] \
    || die "development session lifecycle lock descriptor does not match its path"

  exec {probe_fd}<> "$lock_path"
  if flock -s -n "$probe_fd"; then
    flock -u "$probe_fd"
    exec {probe_fd}>&-
    die "development session lifecycle lock is not held exclusively"
  fi
  exec {probe_fd}>&-
  flock -x -n "$lock_fd" \
    || die "development session lifecycle lock descriptor does not own the exclusive lock"

  local entry journal_name journal_command allowed=false
  devcluster_load_runtime_contract
  for entry in "${DEVCLUSTER_LIFECYCLE_JOURNALS[@]}"; do
    IFS=$'\t' read -r journal_name journal_command <<< "$entry"
    if [ "$journal_name" = "$owner" ] &&
       { [ "$journal_command" = archive ] || [ "$journal_command" = delete ]; }; then
      allowed=true
      break
    fi
  done
  [ "$allowed" = true ] \
    || die "unsupported development session lifecycle owner '$owner'"
}

devcluster_require_lifecycle_mutation_allowed() {
  local slug="$1"
  local operation_name="$2"
  local owner="${VPSFREE_DEV_SESSION_LIFECYCLE_OPERATION:-}"
  local entry operation _command journal

  devcluster_load_runtime_contract
  for entry in "${DEVCLUSTER_LIFECYCLE_JOURNALS[@]}"; do
    IFS=$'\t' read -r operation _command <<< "$entry"
    journal="$WORKSPACE/worktrees/.locks/$slug.$operation.json"
    if [ -e "$journal" ] || [ -L "$journal" ]; then
      [ ! -L "$journal" ] && [ -f "$journal" ] \
        || die "development session '$slug' has an unsafe lifecycle journal"
      [ "$(stat -c %u "$journal")" = "$(id -u)" ] \
        && [ "$(stat -c %a "$journal")" = 600 ] \
        || die "development session '$slug' lifecycle journal must be an owned mode-0600 file"
      [ "$owner" = "$operation" ] \
        || die "development session '$slug' has an unfinished lifecycle operation"
      [ "$operation_name" = reset ] \
        || die "only lifecycle-owned cluster reset is allowed for '$slug'"
      devcluster_require_lifecycle_lock_owner "$slug" "$owner"
    fi
  done
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
  local mutation="$3"
  local operation_name="$4"
  local callback="$5"
  shift 5

  local cluster_exists=false
  if devcluster_validate_cluster_directory "$slug" false; then
    cluster_exists=true
  fi
  if [ "$mutation" = true ]; then
    devcluster_require_lifecycle_mutation_allowed "$slug" "$operation_name"
  fi
  if [ "$require_active" = true ]; then
    devcluster_require_active_session "$slug"
  fi
  if [ "$cluster_exists" = true ] &&
    [ "$operation_name" != reset ] &&
    [ "$operation_name" != transition-adopt ]; then
    devcluster_socket_dir "$slug" "$DEVCLUSTER_SOCKET_PREFIX" >/dev/null
  fi
  "$callback" "$slug" "$@"
}

devcluster_with_lifecycle_lock() {
  local slug="$1"
  local require_active="$2"
  local mutation="$3"
  local operation_name="$4"
  local callback="$5"
  shift 5

  devcluster_with_lock "$DEVCLUSTER_KIND-$slug" \
    devcluster_lifecycle_callback \
    "$slug" "$require_active" "$mutation" "$operation_name" "$callback" "$@"
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
      devcluster_with_lifecycle_lock \
        "$slug" false true gcroots-cleanup gcroot_cluster "$cleanup"
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
  devcluster_socket_dir "$slug" "$DEVCLUSTER_SOCKET_PREFIX" >/dev/null
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
