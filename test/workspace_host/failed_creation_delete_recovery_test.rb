# frozen_string_literal: true

require_relative '../support/workspace_host_test_case'
require_relative 'suspension_test'
load File.expand_path('../../libexec/dev-session', __dir__) unless defined?(DevSession::Runner)

class WorkspaceHostTest < Minitest::Test
  class FailedCreationNoTerminal
    attr_accessor :terminal
    def session(*) = terminal
    def session_by_id(*) = nil
    def managed_session_identities = []
    def current_session_identity(*) = nil
  end

  class FailedCreationConfirmation < StringIO
    def tty? = true
  end

  module FailedCreationFaults
    attr_accessor :fixture_host

    private

    def publish_failed_creation_delete_authority!(slug, session, context)
      if fixture_host.authority_context_change
        path = File.join(work_dir(slug), 'portal.yml')
        record = YAML.safe_load(File.read(path))
        record.fetch('creation')['initial_goal_sent'] = true
        File.write(path, YAML.dump(record))
      end
      super
    end

    def write_session_authority(slug, session, state:)
      super
      fixture_host.authority_publications ||= []
      fixture_host.authority_publications << File.binread(authority_file(slug))
      fail_deletion_fixture!('authority_published')
    end

    def advance_removal!(slug, removal, phase)
      super
      fail_deletion_fixture!(phase)
    end

    def move_to_removal_recovery!(source, recovery, name)
      fail_deletion_fixture!('during_move') if name == 'start.json'
      super
      fail_deletion_fixture!('after_move') if name == 'start.json'
    end

    def preserve_removed_state!(*args)
      super
      fail_deletion_fixture!('tracking_moved')
    end

    def fail_deletion_fixture!(phase)
      return unless fixture_host.deletion_fault == phase && !fixture_host.deletion_fault_fired
      fixture_host.deletion_fault_fired = true
      raise DevSession::Error, "injected deletion failure: #{phase}"
    end
  end

  module FailedCreationHostFixture
    attr_accessor :deletion_fault, :deletion_fault_fired, :confirmation_answer,
                  :turnover_before_lock, :native_turnover_before_lock, :native_turnover_command, :fixture_terminal, :reject_selected_protocol,
                  :live_disposable_terminal, :authority_publications, :authority_terminal_fault, :authority_context_change
    attr_reader :deletion_invocations

    private

    def with_transition_lock(mode = File::LOCK_EX, &block)
      if turnover_before_lock
        self.turnover_before_lock = false
        File.unlink(@profile)
        File.symlink(candidate, @profile)
      end
      if native_turnover_before_lock
        self.native_turnover_before_lock = false
        root = File.join(@state, 'codex', 'current')
        File.unlink(root)
        File.symlink(File.dirname(File.dirname(native_turnover_command)), root)
      end
      super(mode, &block)
    end

    def check_codex(command)
      super
      raise DevWorkspaceHost::Error, 'selected protocol fixture refused' if reject_selected_protocol
    end

    def failed_creation_delete_verifier(entry)
      runner = super
      inject_disposable_terminal(runner)
      runner
    end

    def inject_disposable_terminal(runner)
      if live_disposable_terminal
        if authority_terminal_fault
          fault = authority_terminal_fault
          runner.instance_variable_get(:@tmux).define_singleton_method(:session_by_id) do |id|
            value = super(id)
            value = value.dup if value
            value[fault.fetch(:field)] = fault.fetch(:value) if value
            value
          end
        end
        return runner
      end
      terminal = FailedCreationNoTerminal.new
      terminal.terminal = fixture_terminal
      runner.instance_variable_set(:@tmux, terminal)
      runner.instance_variable_set(:@default_tmux, terminal)
      runner.instance_variable_set(:@tmux_injected, true)
      runner
    end

    def system_env_interactive!(environment, command, *arguments)
      (@deletion_invocations ||= []) << [environment, command, arguments]
      cli = DevSession::CLI.new(arguments, input: FailedCreationConfirmation.new(confirmation_answer || "yes\n"),
                               out: @out, err: @err, env: environment)
      owner = self
      cli.define_singleton_method(:runner) do
        value = super()
        owner.send(:inject_disposable_terminal, value)
        value.extend(FailedCreationFaults)
        value.fixture_host = owner
        value
      end
      status = cli.run
      raise DevWorkspaceHost::Error, 'ordinary deletion executor refused' unless status.zero?
    end
  end

  def test_failed_creation_candidate_missing_authority_uses_original_live_terminal
    with_failed_creation_candidate do |host, paths|
      with_failed_creation_live_terminal(host, paths) do |terminal, authority|
        start = File.binread(paths.fetch(:start))
        refute(File.exist?(authority))
        assert_equal(0, invoke_failed_creation_candidate(host, paths), host.instance_variable_get(:@err).string)
        assert_equal(1, host.authority_publications.length)
        record = JSON.parse(host.authority_publications.first)
        assert_equal('creating', record.fetch('state'))
        assert_equal(terminal.fetch(:id), record.fetch('tmux_session_id'))
        assert_equal('d' * 64, record.fetch('tmux_identity'))
        assert_equal(paths.fetch(:root_thread), record.fetch('codex_thread_id'))
        recovery = completed_failed_creation_recovery(host, paths)
        assert_equal(start, File.binread(File.join(recovery, 'start.json')))
        refute(File.exist?(authority))
        assert_equal(0, invoke_failed_creation_candidate(host, paths))
        assert_equal(1, host.deletion_invocations.length)
        assert_equal(1, host.authority_publications.length)
        calls = File.readlines(paths.fetch(:calls), chomp: true).map { |line| JSON.parse(line) }
        refute(calls.any? { |call| (call & %w[start create resume bootstrap ensure-initial project/delete]).any? })
      end
    end
  end

  def test_failed_creation_candidate_authority_publication_interruption_reuses_exact_record
    with_failed_creation_candidate do |host, paths|
      with_failed_creation_live_terminal(host, paths) do |_terminal, authority|
        original = %i[start creation roster].to_h { |key| [key, File.binread(paths.fetch(key))] }
        host.deletion_fault = 'authority_published'
        assert_equal(1, invoke_failed_creation_candidate(host, paths))
        assert(host.deletion_fault_fired)
        bytes = File.binread(authority)
        assert_equal(0o600, File.stat(authority).mode & 0o777)
        assert_equal('creating', JSON.parse(bytes).fetch('state'))
        assert_empty(Dir.glob(File.join(File.dirname(paths.fetch(:start)), '*.removal.json')))
        original.each { |key, value| assert_equal(value, File.binread(paths.fetch(key))) }
        assert_equal(0, invoke_failed_creation_candidate(host, paths), host.instance_variable_get(:@err).string)
        assert_equal([bytes], host.authority_publications, 'replay must not replace the creating authority')
        completed_failed_creation_recovery(host, paths)
        refute(File.exist?(authority))
      end
    end
  end

  def test_failed_creation_candidate_authority_rejects_changed_terminal_tuple
    {id: '$999999', name: 'other', mark: '0', slug: 'other', environment_slug: 'other',
     workspace: '/wrong-workspace', socket_path: '/wrong-tmux.sock', identity_token: 'e' * 64,
     codex_thread_id: '99999999-1111-7111-8111-111111111111', codex_socket_path: '/wrong-native.sock',
     codex_client_version: '0.0.1'}.each do |field, value|
      with_failed_creation_candidate do |host, paths|
        with_failed_creation_live_terminal(host, paths) do |_terminal, authority|
          host.authority_terminal_fault = {field:, value:}
          assert_equal(1, invoke_failed_creation_candidate(host, paths), field)
          assert_nil(host.authority_publications, field)
          refute(File.exist?(authority), field)
          assert(File.directory?(paths.fetch(:tracking)), field)
          assert_empty(Dir.glob(File.join(File.dirname(paths.fetch(:start)), '*.removal.json')))
        end
      end
    end
  end

  def test_failed_creation_candidate_authority_rechecks_context_before_publication
    with_failed_creation_candidate do |host, paths|
      with_failed_creation_live_terminal(host, paths) do |_terminal, authority|
        host.authority_context_change = true
        assert_equal(1, invoke_failed_creation_candidate(host, paths))
        refute(File.exist?(authority))
        assert_nil(host.authority_publications)
        assert_empty(Dir.glob(File.join(File.dirname(paths.fetch(:start)), '*.removal.json')))
      end
    end
  end

  def test_failed_creation_candidate_authority_never_overwrites_existing_record
    %w[malformed conflicting].each do |kind|
      with_failed_creation_candidate do |host, paths|
        with_failed_creation_live_terminal(host, paths) do |terminal, authority|
          bytes = if kind == 'malformed'
                    '{'
                  else
                    JSON.generate('schema' => 1, 'state' => 'creating', 'slug' => paths.fetch(:slug),
                      'workspace' => paths.fetch(:entry).fetch('root'), 'tmux_socket' => terminal.fetch(:socket),
                      'tmux_session_id' => terminal.fetch(:id), 'tmux_identity' => 'e' * 64,
                      'codex_thread_id' => paths.fetch(:root_thread), 'codex_socket_path' => terminal.fetch(:native),
                      'codex_client_version' => '1.2.3')
                  end
          FileUtils.mkdir_p(File.dirname(authority), mode: 0o700)
          File.write(authority, bytes)
          File.chmod(0o600, authority)
          assert_equal(1, invoke_failed_creation_candidate(host, paths), kind)
          assert_equal(bytes, File.binread(authority), kind)
          assert_nil(host.authority_publications, kind)
          assert_nil(host.deletion_invocations, kind)
        end
      end
    end
  end

  def test_failed_creation_candidate_absent_terminal_does_not_publish_authority
    with_failed_creation_candidate do |host, paths|
      authority = File.join(host.send(:instance_runtime, paths.fetch(:entry)).fetch(:authority), paths.fetch(:slug) + '.json')
      assert_equal(0, invoke_failed_creation_candidate(host, paths))
      assert_nil(host.authority_publications)
      refute(File.exist?(authority))
    end
  end

  def test_failed_creation_candidate_public_entry_runs_ordinary_deletion_and_exact_replay
    with_failed_creation_candidate do |host, paths|
      initial = %i[start creation roster].to_h { |key| [key, File.binread(paths.fetch(key))] }
      token = host.send(:profile_link_token)
      assert_equal(0, invoke_failed_creation_candidate(host, paths), host.instance_variable_get(:@err).string)
      recovery = completed_failed_creation_recovery(host, paths)
      assert_equal(initial[:start], File.binread(File.join(recovery, 'start.json')))
      assert_equal(initial[:creation], File.binread(File.join(recovery, 'creation.json')))
      assert_equal(initial[:roster], File.binread(File.join(recovery, 'team.json')))
      assert_equal('removed', JSON.parse(File.read(File.join(recovery, 'recovery.json')))['phase'])
      refute(File.exist?(paths.fetch(:tracking)))
      refute(File.exist?(paths.fetch(:start)))
      assert_equal(token, host.send(:profile_link_token))
      assert_equal(paths.fetch(:predecessor), File.realpath(host.instance_variable_get(:@profile)))
      assert_equal(1, host.deletion_invocations.length)
      _, command, args = host.deletion_invocations.first
      assert_equal(File.join(paths.fetch(:candidate), 'libexec/workspace-portal/dev-session'), command)
      assert_equal(paths.fetch(:predecessor), args.fetch(args.index('--expected-host-generation') + 1))
      assert_equal(['delete', paths.fetch(:slug), '--as-is'], args[(args.index('--') + 1)..])
      refute_includes(args, '--transition-lock')
      refute_includes(args, '--force')
      assert_equal(0, invoke_failed_creation_candidate(host, paths))
      assert_equal(1, host.deletion_invocations.length, 'completed replay must not start another deletion')
      assert_equal(token, host.send(:profile_link_token))
      refute(host.events.any? { |e| %i[profile_set configured consumers_restarted].include?(e.first) })
      calls = File.readlines(paths.fetch(:calls), chomp: true).map { |line| JSON.parse(line) }
      refute(calls.any? { |call| (call & %w[start create bootstrap ensure-initial project/delete]).any? })
      assert(calls.any? { |call| call[0, 2] == %w[team prepare-delete] })
      assert(calls.any? { |call| call[0, 2] == %w[thread retire] })
      lock = File.open(host.send(:transition_lock_file), File::RDWR)
      assert(lock.flock(File::LOCK_EX | File::LOCK_NB), 'host must release exclusion before portal reconciliation')
      lock.close
      error = assert_raises(DevWorkspaceHost::Error) { host.send(:require_no_unfinished_lifecycle_operations!) }
      assert_includes(error.message, 'portal direct creation', 'ordinary portal still owns the failed receipt')
    end
  end

  def test_failed_creation_candidate_reused_slug_binds_current_epoch_and_preserves_history
    with_failed_creation_candidate do |host, paths|
      previous = {}
      3.times do |index|
        current = %i[start creation roster].to_h { |key| [key, File.binread(paths.fetch(key))] }
        before = Dir.glob(File.join(host.instance_variable_get(:@state), 'removed', paths.fetch(:workspace_id), '*'))
        assert_equal(0, invoke_failed_creation_candidate(host, paths), "history=#{index}: #{host.instance_variable_get(:@err).string}")
        after = Dir.glob(File.join(host.instance_variable_get(:@state), 'removed', paths.fetch(:workspace_id), '*'))
        added = after - before
        assert_equal(1, added.length, 'each accepted creation owns one new removal')
        recovery = added.fetch(0)
        %i[start creation roster].zip(%w[start.json creation.json team.json]).each do |key, name|
          assert_equal(current.fetch(key), File.binread(File.join(recovery, name)))
        end
        assert_equal(index + 1, host.deletion_invocations.length)
        assert_equal(0, invoke_failed_creation_candidate(host, paths), 'completed replay binds only the current epoch')
        assert_equal(index + 1, host.deletion_invocations.length, 'replay must not create another removal')
        previous.each { |path, bytes| assert_equal(bytes, File.binread(path), path) }
        Dir.glob(File.join(recovery, '**', '*'), File::FNM_DOTMATCH).select { |path| File.file?(path) }.each do |path|
          previous[path] = File.binread(path)
        end
        previous[paths.fetch(:preparation_path)] = File.binread(paths.fetch(:preparation_path))
        renew_failed_creation_candidate(paths, index + 1) if index < 2
      end
    end
  end

  def test_failed_creation_candidate_reused_slug_interruption_keeps_current_operation
    with_failed_creation_candidate do |host, paths|
      assert_equal(0, invoke_failed_creation_candidate(host, paths))
      old_bytes = {}
      2.times do |index|
        Dir.glob(File.join(paths.fetch(:removal_root), '*')).each do |recovery|
          old_bytes.merge!(failed_creation_recovery_bytes(recovery))
        end
        old_bytes[paths.fetch(:preparation_path)] = File.binread(paths.fetch(:preparation_path))
        renew_failed_creation_candidate(paths, index + 1)
        host.deletion_fault_fired = false
        host.deletion_fault = 'tracking_moved'
        assert_equal(1, invoke_failed_creation_candidate(host, paths))
        removal_path = paths.fetch(:start).sub('.start.json', '.removal.json')
        removal = JSON.parse(File.read(removal_path))
        refute(old_bytes.key?(File.join(removal.fetch('recovery'), 'recovery.json')))
        refute(File.exist?(paths.fetch(:tracking)))
        assert_equal(0, invoke_failed_creation_candidate(host, paths), host.instance_variable_get(:@err).string)
        completed = JSON.parse(File.read(File.join(removal.fetch('recovery'), 'recovery.json')))
        assert_equal(removal.fetch('operation_id'), completed.fetch('operation_id'))
        count = host.deletion_invocations.length
        assert_equal(0, invoke_failed_creation_candidate(host, paths))
        assert_equal(count, host.deletion_invocations.length)
        old_bytes.each { |path, bytes| assert_equal(bytes, File.binread(path), path) }
      end
    end
  end

  def test_failed_creation_candidate_replay_refuses_changed_or_ambiguous_history
    %w[extra_operation duplicate_operation malformed_history wrong_private_root].each do |kind|
      with_failed_creation_candidate do |host, paths|
        assert_equal(0, invoke_failed_creation_candidate(host, paths))
        recovery = completed_failed_creation_recovery(host, paths)
        preserved = failed_creation_recovery_bytes(recovery)
        if kind == 'wrong_private_root'
          path = File.join(recovery, 'work', 'portal.yml')
          manifest = YAML.safe_load(File.read(path))
          manifest.fetch('codex')['thread_id'] = '99999999-1111-7111-8111-111111111111'
          File.write(path, YAML.dump(manifest))
          preserved[path] = File.binread(path)
        else
          other = recovery + '-additional'
          FileUtils.cp_r(recovery, other)
          File.chmod(0o700, other)
          marker = File.join(other, 'recovery.json')
          metadata = JSON.parse(File.read(marker))
          metadata['recovery'] = other
          metadata['operation_id'] = '0' * 64 if kind == 'extra_operation'
          File.write(marker, kind == 'malformed_history' ? '{' : JSON.generate(metadata))
          File.chmod(0o600, marker)
          preserved.merge!(failed_creation_recovery_bytes(other))
        end
        count = host.deletion_invocations.length
        assert_equal(1, invoke_failed_creation_candidate(host, paths), kind)
        assert_equal(count, host.deletion_invocations.length, kind)
        preserved.each { |path, bytes| assert_equal(bytes, File.binread(path), path) }
      end
    end
  end

  def test_failed_creation_candidate_reused_slug_preserves_exact_preparation_checks
    %w[explicit_old_request malformed_old_preparation current_ambiguity].each do |kind|
      with_failed_creation_candidate do |host, paths|
        assert_equal(0, invoke_failed_creation_candidate(host, paths))
        previous = paths.dup
        old_preparation = File.binread(paths.fetch(:preparation_path))
        renew_failed_creation_candidate(paths, 1)
        if kind == 'explicit_old_request'
          paths[:request] = previous.fetch(:request)
        elsif kind == 'malformed_old_preparation'
          File.write(previous.fetch(:preparation_path), '{')
        else
          other = JSON.parse(File.read(paths.fetch(:preparation_path)))
          other['requestId'] = '99999999-1111-4111-8111-111111111111'
          path = File.join(File.dirname(paths.fetch(:preparation_path)), "#{other.fetch('requestId')}.json")
          File.write(path, JSON.generate(other))
          File.chmod(0o600, path)
        end
        assert_equal(1, invoke_failed_creation_candidate(host, paths), kind)
        assert(File.directory?(paths.fetch(:tracking)))
        if kind != 'malformed_old_preparation'
          assert_equal(old_preparation, File.binread(previous.fetch(:preparation_path)))
        end
      end
    end
  end

  def test_failed_creation_candidate_prior_history_does_not_prove_missing_current_removal
    with_failed_creation_candidate do |host, paths|
      assert_equal(0, invoke_failed_creation_candidate(host, paths))
      renew_failed_creation_candidate(paths, 1)
      tracking = paths.fetch(:tracking)
      displaced = tracking + '-displaced'
      File.rename(tracking, displaced)
      count = host.deletion_invocations.length
      assert_equal(1, invoke_failed_creation_candidate(host, paths), 'old history cannot prove current deletion')
      assert_equal(count, host.deletion_invocations.length)
      assert(File.file?(paths.fetch(:start)))
      File.rename(displaced, tracking)
      receipt = JSON.parse(File.read(paths.fetch(:receipt_path)))
      receipt['deletionHistorySha256'] = Digest::SHA256.hexdigest('')
      File.write(paths.fetch(:receipt_path), JSON.generate(receipt))
      preparation = JSON.parse(File.read(paths.fetch(:preparation_path)))
      preparation['epoch'] = receipt.fetch('deletionHistorySha256')
      File.write(paths.fetch(:preparation_path), JSON.generate(preparation))
      assert_equal(1, invoke_failed_creation_candidate(host, paths), 'live tracking cannot replay an old epoch')
      assert_equal(count, host.deletion_invocations.length)
    end
  end

  def test_failed_creation_candidate_interrupted_ordinary_deletion_replays_one_operation
    %w[validated thread_retiring during_move after_move tracking_moved].each do |phase|
      with_failed_creation_candidate do |host, paths|
        bytes = File.binread(paths.fetch(:start))
        host.deletion_fault = phase
        assert_equal(1, invoke_failed_creation_candidate(host, paths), phase)
        assert(host.deletion_fault_fired, "fault was not reached: #{phase}: #{host.instance_variable_get(:@err).string}")
        journals = Dir.glob(File.join(File.dirname(paths.fetch(:start)), '*.removal.json'))
        if phase == 'validated'
          assert_equal(bytes, File.binread(paths.fetch(:start)))
          assert_empty(journals, 'pre-retirement failure keeps ordinary discard behavior')
        else
          assert_equal(1, journals.length)
          operation = JSON.parse(File.read(journals.first)).fetch('operation_id')
          assert_equal(0, invoke_failed_creation_candidate(host, paths), "#{phase}: #{host.instance_variable_get(:@err).string}")
          recovery = completed_failed_creation_recovery(host, paths)
          assert_equal(operation, JSON.parse(File.read(File.join(recovery, 'recovery.json'))).fetch('operation_id'))
          assert_equal(bytes, File.binread(File.join(recovery, 'start.json')))
          next
        end
        assert_equal(0, invoke_failed_creation_candidate(host, paths), host.instance_variable_get(:@err).string)
      end
    end
  end

  def test_failed_creation_candidate_refuses_unconfirmed_or_changed_context_before_effects
    %w[request root attempted missing_sent nonce start_slug oversized duplicate start_missing receipt_ready preparation_receipt member_project member_bootstrap fork archive migration authority terminal].each do |kind|
      with_failed_creation_candidate do |host, paths|
        case kind
        when 'request' then paths = paths.merge(request: '99999999-9999-4999-8999-999999999999')
        when 'root', 'attempted', 'missing_sent'
          x = YAML.safe_load(File.read(paths.fetch(:manifest)))
          x['codex']['thread_id'] = '22222222-2222-7222-8222-222222222222' if kind == 'root'
          x['creation']['initial_goal_attempted'] = true if kind == 'attempted'
          x['creation'].delete('initial_goal_sent') if kind == 'missing_sent'
          File.write(paths.fetch(:manifest), YAML.dump(x))
        when 'nonce', 'start_slug'
          x = JSON.parse(File.read(paths.fetch(:start)))
          x[kind == 'nonce' ? 'tmux_identity' : 'slug'] = kind == 'nonce' ? 'f' * 64 : 'wrong'
          File.write(paths.fetch(:start), JSON.generate(x))
        when 'oversized' then File.write(paths.fetch(:start), 'x' * 65_537)
        when 'duplicate' then File.write(paths.fetch(:start), '{"schema":1,"schema":1}')
        when 'start_missing' then File.unlink(paths.fetch(:start))
        when 'receipt_ready'
          path = paths.fetch(:receipt_path)
          x = JSON.parse(File.read(path)); x['state'] = 'ready'
          File.write(path, JSON.generate(x))
        when 'preparation_receipt'
          path = paths.fetch(:preparation_path)
          x = JSON.parse(File.read(path)); x['receiptId'] = '0' * 64
          File.write(path, JSON.generate(x))
        when 'member_project', 'member_bootstrap'
          x = JSON.parse(File.read(paths.fetch(:roster)))
          x['members'].first[kind == 'member_project' ? 'projectId' : 'bootstrapAttempted'] = kind == 'member_project' ? 'legacy' : true
          File.write(paths.fetch(:roster), JSON.generate(x))
        when 'fork', 'archive', 'migration'
          suffix = {'fork' => 'fork', 'archive' => 'archive', 'migration' => 'agent-teams-migration'}.fetch(kind)
          File.write(File.join(File.dirname(paths.fetch(:start)), "#{paths.fetch(:slug)}.#{suffix}.json"), '{}')
        when 'terminal'
          host.fixture_terminal = DevSession::Tmux::Session.new(id: '$fake', name: paths.fetch(:slug),
            mark: '1', slug: paths.fetch(:slug), workspace: paths.fetch(:entry).fetch('root'),
            identity_token: 'f' * 64, codex_thread_id: paths.fetch(:root_thread),
            codex_socket_path: host.send(:instance_runtime, paths.fetch(:entry)).fetch(:codex))
        when 'authority'
          directory = host.send(:instance_runtime, paths.fetch(:entry)).fetch(:authority)
          FileUtils.mkdir_p(directory)
          File.write(File.join(directory, paths.fetch(:slug) + '.json'), '{}')
          File.chmod(0o600, File.join(directory, paths.fetch(:slug) + '.json'))
        end
        assert_equal(1, invoke_failed_creation_candidate(host, paths), kind)
        assert_nil(host.deletion_invocations, kind)
        assert(File.directory?(paths.fetch(:tracking)), kind)
        assert_equal(paths.fetch(:predecessor), File.realpath(host.instance_variable_get(:@profile)), kind)
      end
    end
  end

  def test_failed_creation_candidate_source_profile_and_contract_are_exact
    %w[source turnover native protocol contract policy_zero policy_string current_contract].each do |kind|
      with_failed_creation_candidate do |host, paths|
        case kind
        when 'source' then host.executing_package = paths.fetch(:predecessor)
        when 'turnover' then host.turnover_before_lock = true
        when 'protocol' then host.reject_selected_protocol = true
        when 'native'
          host.native_turnover_command = make_codex(paths.fetch(:root), 'native-turnover')
          host.native_turnover_before_lock = true
        else
          package = kind == 'current_contract' ? paths.fetch(:candidate) : paths.fetch(:predecessor)
          path = File.join(package, 'share/workspace-portal/runtime-contract.json')
          x = JSON.parse(File.read(path))
          case kind
          when 'contract', 'current_contract' then x['trackingMaxBytes'] += 1
          when 'policy_zero' then x['turnRetrySessionPolicy'] = 0
          when 'policy_string' then x['turnRetrySessionPolicy'] = '1'
          end
          File.write(path, JSON.generate(x))
        end
        assert_equal(1, invoke_failed_creation_candidate(host, paths), kind)
        assert_nil(host.deletion_invocations, kind)
      end
    end
  end

  def test_failed_creation_candidate_preserves_ordinary_confirmation
    with_failed_creation_candidate do |host, paths|
      host.confirmation_answer = "no\n"
      assert_equal(1, invoke_failed_creation_candidate(host, paths))
      assert(File.exist?(paths.fetch(:start)))
      assert(File.directory?(paths.fetch(:tracking)))
      assert_empty(Dir.glob(File.join(File.dirname(paths.fetch(:start)), '*.removal.json')))
    end
  end

  def test_failed_creation_candidate_native_retirement_failure_retains_start_and_operation
    with_failed_creation_candidate do |host, paths|
      bytes = File.binread(paths.fetch(:start))
      File.write(paths.fetch(:fault_marker), 'fail')
      assert_equal(1, invoke_failed_creation_candidate(host, paths))
      path = paths.fetch(:start).sub('.start.json', '.removal.json')
      removal = JSON.parse(File.read(path))
      assert_equal('thread_retiring', removal.fetch('phase'))
      refute(File.exist?(paths.fetch(:start)))
      assert_equal(bytes, File.binread(File.join(removal.fetch('recovery'), 'start.json')))
      File.unlink(paths.fetch(:fault_marker))
      assert_equal(0, invoke_failed_creation_candidate(host, paths), host.instance_variable_get(:@err).string)
      completed = JSON.parse(File.read(File.join(removal.fetch('recovery'), 'recovery.json')))
      assert_equal(removal.fetch('operation_id'), completed.fetch('operation_id'))
    end
  end

  def test_failed_creation_candidate_replay_refuses_missing_or_conflicting_start_provenance
    %w[missing conflicting].each do |kind|
      with_failed_creation_candidate do |host, paths|
        host.deletion_fault = 'after_move'
        assert_equal(1, invoke_failed_creation_candidate(host, paths))
        removal_path = paths.fetch(:start).sub('.start.json', '.removal.json')
        removal = JSON.parse(File.read(removal_path))
        private_start = File.join(removal.fetch('recovery'), 'start.json')
        if kind == 'missing'
          File.unlink(private_start)
        else
          File.write(paths.fetch(:start), File.binread(private_start))
          File.chmod(0o600, paths.fetch(:start))
        end
        count = host.deletion_invocations.length
        assert_equal(1, invoke_failed_creation_candidate(host, paths), kind)
        assert_equal(count, host.deletion_invocations.length, 'invalid provenance reached ordinary executor')
        assert_equal(removal.fetch('operation_id'), JSON.parse(File.read(removal_path)).fetch('operation_id'))
      end
    end
  end

  def test_failed_creation_candidate_accepts_equal_policy_one_contract_without_selecting
    with_failed_creation_candidate do |host, paths|
      path = File.join(paths.fetch(:predecessor), 'share/workspace-portal/runtime-contract.json')
      File.write(path, JSON.generate(DevWorkspaceHost::RUNTIME_CONTRACT))
      token = host.send(:profile_link_token)
      assert_equal(0, invoke_failed_creation_candidate(host, paths), host.instance_variable_get(:@err).string)
      assert_equal(token, host.send(:profile_link_token))
    end
  end

  private

  def with_failed_creation_live_terminal(host, paths)
    runtime = host.send(:instance_runtime, paths.fetch(:entry))
    socket = runtime.fetch(:tmux)
    FileUtils.mkdir_p(File.dirname(socket))
    shell = host.instance_variable_get(:@env).fetch('SHELL', ENV.fetch('SHELL', '/bin/sh'))
    target = "=#{paths.fetch(:slug)}:"
    command = lambda do |*args|
      output, error, status = Open3.capture3({'TMUX' => nil}, 'tmux', '-S', socket, *args)
      assert(status.success?, "disposable tmux #{args.first} failed: #{error}")
      output.strip
    end
    command.call('-f', '/dev/null', 'new-session', '-d', '-s', paths.fetch(:slug), '-c', paths.fetch(:tracking), shell)
    id = command.call('display-message', '-p', '-t', target, '#{session_id}')
    pane = command.call('display-message', '-p', '-t', target, '#{pane_id}')
    {'@dev_session' => '1', '@dev_session_slug' => paths.fetch(:slug),
     '@dev_session_codex_thread' => paths.fetch(:root_thread), '@dev_session_codex_socket' => runtime.fetch(:codex),
     '@dev_session_codex_version' => '1.2.3', '@dev_session_codex_pane' => pane}.each do |key, value|
      command.call('set-option', '-t', target, key, value)
    end
    {'DEV_SESSION_WORKSPACE' => paths.fetch(:entry).fetch('root'), 'DEV_SESSION_SLUG' => paths.fetch(:slug),
     'DEV_SESSION_TMUX_IDENTITY' => 'd' * 64}.each do |key, value|
      command.call('set-environment', '-t', target, key, value)
    end
    host.live_disposable_terminal = true
    authority = File.join(runtime.fetch(:authority), paths.fetch(:slug) + '.json')
    yield({id:, socket:, native: runtime.fetch(:codex)}, authority)
  ensure
    Open3.capture3('tmux', '-S', socket, 'kill-server') if socket
  end

  def failed_creation_recovery_bytes(recovery)
    Dir.glob(File.join(recovery, '**', '*'), File::FNM_DOTMATCH).select { |path| File.file?(path) }.to_h do |path|
      [path, File.binread(path)]
    end
  end

  def renew_failed_creation_candidate(paths, index)
    root = paths.fetch(:removal_root)
    recoveries = Dir.glob(File.join(root, '*'))
    previous = recoveries.find { |path| YAML.safe_load(File.read(File.join(path, 'work', 'portal.yml'))).dig('codex', 'thread_id') == paths.fetch(:root_thread) }
    raise 'previous disposable creation has no preserved tracking' unless previous

    FileUtils.cp_r(File.join(previous, 'work'), paths.fetch(:tracking))
    %i[start creation roster].zip(%w[start.json creation.json team.json]).each do |key, name|
      FileUtils.cp(File.join(previous, name), paths.fetch(key))
      File.chmod(0o600, paths.fetch(key))
    end
    operations = recoveries.map { |path| JSON.parse(File.read(File.join(path, 'recovery.json'))).fetch('operation_id') }
    epoch = Digest::SHA256.hexdigest(operations.uniq.sort.join("\n"))
    request = format('%08x-1111-4111-8111-111111111111', index + 1)
    thread = format('%08x-1111-7111-8111-111111111111', index + 1)
    nonce = Digest::SHA256.hexdigest("disposable creation #{index}")
    manifest = YAML.safe_load(File.read(paths.fetch(:manifest)))
    manifest.fetch('codex')['thread_id'] = thread
    File.write(paths.fetch(:manifest), YAML.dump(manifest))
    %i[start creation].each do |key|
      record = JSON.parse(File.read(paths.fetch(key)))
      record['tmux_identity'] = nonce
      File.write(paths.fetch(key), JSON.generate(record))
    end
    roster = JSON.parse(File.read(paths.fetch(:roster)))
    roster['rootThreadId'] = thread
    roster.fetch('members').first['projectId'] = format('%08x-2222-7222-8222-222222222222', index + 1)
    File.write(paths.fetch(:roster), JSON.generate(roster))
    receipt = JSON.parse(File.read(paths.fetch(:receipt_path)))
    receipt['receiptId'] = Digest::SHA256.hexdigest("disposable receipt #{index}")
    receipt['deletionHistorySha256'] = epoch
    File.write(paths.fetch(:receipt_path), JSON.generate(receipt))
    preparation = JSON.parse(File.read(paths.fetch(:preparation_path)))
    preparation.merge!('requestId' => request, 'receiptId' => receipt.fetch('receiptId'), 'epoch' => epoch)
    paths[:request] = request
    paths[:root_thread] = thread
    paths[:preparation_path] = File.join(File.dirname(paths.fetch(:preparation_path)), "#{request}.json")
    File.write(paths.fetch(:preparation_path), JSON.generate(preparation))
    File.chmod(0o600, paths.fetch(:preparation_path))
  end

  def invoke_failed_creation_candidate(host, paths)
    host.run('workspace-host', ['recover-failed-creation-delete', '--source', paths.fetch(:source),
      '--workspace', 'example-workspace', '--session', paths.fetch(:slug), '--request-id', paths.fetch(:request)])
  end

  def completed_failed_creation_recovery(host, paths)
    root = File.join(host.instance_variable_get(:@state), 'removed', paths.fetch(:workspace_id))
    matches = Dir.glob(File.join(root, "*-#{paths.fetch(:slug)}-*"))
    assert_equal(1, matches.length)
    matches.first
  end

  def with_failed_creation_candidate
    with_transition_host do |host, paths|
      host.send(:root_codex, paths.fetch(:old_codex), paths.fetch(:current_root))
      assert_equal(0, host.run('workspace-host', ['switch', '--source', paths.fetch(:source)]))
      predecessor = File.realpath(host.instance_variable_get(:@profile))
      candidate = make_package(paths.fetch(:root), 'failed-creation-candidate')
      host.candidate = candidate
      host.executing_package = candidate
      host.extend(FailedCreationHostFixture)
      [predecessor, candidate].each do |package|
        contract = Marshal.load(Marshal.dump(DevWorkspaceHost::RUNTIME_CONTRACT))
        contract.delete('turnRetrySessionPolicy') if package == predecessor
        File.write(File.join(package, 'share/workspace-portal/runtime-contract.json'), JSON.generate(contract))
        executor = File.join(package, 'libexec/workspace-portal/dev-session')
        FileUtils.mkdir_p(File.dirname(executor))
        File.write(executor, "#!/bin/sh\nexit 0\n")
        File.chmod(0o755, executor)
      end
      entry = host.send(:registry).find('example-workspace')
      workspace = entry.fetch('root')
      slug = '2026-10-24-failed-recovery'
      request = '11111111-1111-4111-8111-111111111111'
      root_thread = '11111111-1111-7111-8111-111111111111'
      workspace_id = "#{File.basename(workspace)}-#{Digest::SHA256.hexdigest(workspace)[0, 16]}"
      state = host.instance_variable_get(:@state)
      portal = File.join(state, 'portal', workspace_id)
      preset = {'id' => 'delegated', 'name' => 'Team', 'description' => 'Disposable team',
        'catalogDigest' => 'a' * 64, 'teamDigest' => 'b' * 64, 'roles' => %w[lead reviewer0],
        'leadModel' => 'gpt-6.1-sol', 'leadEffort' => 'xhigh', 'leadInstructions' => 'Lead the task.',
        'members' => [{'address' => 'reviewer0', 'role' => 'reviewer', 'behavior' => 'reviewer',
          'purpose' => 'review', 'access' => 'read_only', 'model' => 'gpt-6.1-sol',
          'reasoningEffort' => 'xhigh', 'instructions' => 'Review assigned changes.'}]}
      nonce = 'd' * 64
      goal = "Disposable initial request.\n"
      receipt_id = 'e' * 64
      epoch = Digest::SHA256.hexdigest('')
      accepted = {'kind' => 'new', 'slug' => slug, 'goal' => goal, 'team' => preset['id'], 'catalogDigest' => preset['catalogDigest']}
      receipt = {'schema' => 3, 'workspace' => workspace, 'request' => accepted, 'receiptId' => receipt_id,
        'deletionHistorySha256' => epoch, 'attempt' => 2, 'state' => 'failed', 'phase' => 'initializing',
        'error' => 'Unknown reviewer start.', 'validated' => true, 'goal' => goal,
        'model' => preset['leadModel'], 'effort' => preset['leadEffort'], 'directTeam' => preset,
        'startedAt' => '2026-10-24T10:00:00Z', 'updatedAt' => '2026-10-24T10:00:01Z'}
      preparation = {'schema' => 1, 'workspace' => workspace, 'requestId' => request, 'inputVersion' => 1,
        'inputDigest' => 'f' * 64, 'snapshotDigest' => 'a' * 64, 'receiptId' => receipt_id,
        'attempt' => 1, 'state' => 'handed_off', 'phase' => 'initializing', 'slug' => slug, 'epoch' => epoch,
        'snapshot' => {'input' => {'rawPrompt' => goal}, 'goal' => goal, 'preset' => preset}, 'handoff' => accepted}
      runtime = host.send(:instance_runtime, entry)
      tracking = File.join(workspace, 'work', slug)
      FileUtils.mkdir_p(tracking)
      File.write(File.join(tracking, 'plan.md'), "# Disposable task\n")
      File.write(File.join(tracking, 'state.md'), "---\nlifecycle: active\n---\n# State\n")
      manifest = File.join(tracking, 'portal.yml')
      File.write(manifest, YAML.dump('schema' => 2, 'slug' => slug,
        'codex' => {'thread_id' => root_thread, 'socket_path' => runtime.fetch(:codex), 'client_version' => '1.2.3'},
        'creation' => {'state' => 'creating', 'initial_goal_attempted' => false, 'initial_goal_sent' => false,
                       'goal_sha256' => Digest::SHA256.hexdigest(goal)}, 'repositories' => [], 'artifacts' => []))
      locks = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(locks)
      creation = File.join(locks, slug + '.creation.json')
      start = File.join(locks, slug + '.start.json')
      roster = File.join(state, 'codex-teams', workspace_id, slug + '.json')
      member = preset['members'].first.merge('index' => 0, 'threadId' => '', 'state' => 'creating',
        'projectId' => '22222222-2222-7222-8222-222222222222', 'createAttempted' => true,
        'addedAt' => '2026-10-24T10:00:00Z')
      records = {
        File.join(portal, 'creations', slug + '.json') => receipt,
        File.join(portal, 'session-preparations', request + '.json') => preparation,
        creation => {'schema' => 3, 'slug' => slug, 'goal_sha256' => Digest::SHA256.hexdigest(goal),
          'run_codex' => true, 'model' => preset['leadModel'], 'effort' => preset['leadEffort'],
          'direct_team' => preset, 'state' => 'creating', 'tmux_identity' => nonce},
        start => {'schema' => 1, 'slug' => slug, 'state' => 'creating', 'tmux_identity' => nonce},
        roster => {'schema' => 1, 'workspace' => workspace, 'slug' => slug, 'rootThreadId' => root_thread,
          'revision' => 1, 'createdAt' => '2026-10-24T10:00:00Z', 'updatedAt' => '2026-10-24T10:00:01Z', 'presetId' => preset['id'], 'catalogDigest' => preset['catalogDigest'],
          'teamDigest' => preset['teamDigest'], 'leadModel' => preset['leadModel'], 'leadEffort' => preset['leadEffort'],
          'leadInstructions' => preset['leadInstructions'], 'members' => [member]}
      }
      records.each do |path, object|
        FileUtils.mkdir_p(File.dirname(path), mode: 0o700)
        File.write(path, JSON.generate(object) + "\n")
        File.chmod(0o600, path)
      end
      calls = File.join(paths.fetch(:root), 'external-calls.jsonl')
      fault_marker = File.join(paths.fetch(:root), 'native-retirement-failure')
      # Ruby owns the real admission, phase writers, atomic moves and completion.
      # The external portal adapter checks call ordering; Go deletion tests own
      # unknown-reservation tombstones and confirmed native-thread retirement.
      helper = File.join(candidate, 'bin/workspace-portal')
      File.write(helper, <<~SCRIPT)
        #!#{RbConfig.ruby}
        require 'json'
        File.open(#{calls.dump}, 'a') { |file| file.puts JSON.generate(ARGV) }
        abort 'injected native retirement failure' if ARGV[0, 2] == ['thread', 'retire'] && File.exist?(#{fault_marker.dump})
        if ARGV[0, 2] == ['team', 'prepare-delete']
          record = JSON.parse(File.read(#{File.join(locks, slug + '.removal.json').dump}))
          abort 'wrong ordinary removal phase' unless record.fetch('phase') == (ARGV.include?('--check-only') ? 'validated' : 'thread_retiring')
        end
      SCRIPT
      File.chmod(0o755, helper)
      cluster = File.join(paths.fetch(:root), 'disposable-cluster')
      File.write(cluster, "#!/bin/sh\nif [ \"$1\" = cleanup-paths ]; then printf '%s\\n' '{\"schema\":1,\"paths\":[]}'; fi\n")
      File.chmod(0o755, cluster)
      catalog_path = host.instance_variable_get(:@env).fetch('DEV_WORKSPACES_EXTENSION_CATALOG')
      catalog = JSON.parse(File.read(catalog_path))
      catalog['clusterProviders'].each { |provider| provider['command'] = cluster }
      File.write(catalog_path, JSON.generate(catalog))
      host.events.clear
      yield host, paths.merge(predecessor:, candidate:, entry:, slug:, request:, workspace_id:,
                             tracking:, manifest:, start:, creation:, roster:, calls:, fault_marker:, root_thread:,
                             removal_root: File.join(state, 'removed', workspace_id),
                             receipt_path: File.join(portal, 'creations', slug + '.json'),
                             preparation_path: File.join(portal, 'session-preparations', request + '.json'))
    end
  end
end
