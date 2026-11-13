# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_ordinary_retained_shape_distinguishes_absent_from_present_creation
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-06-06-ordinary'
      manifest = runner.send(:new_threadless_portal_manifest, slug).merge(
        'codex' => { 'thread_id' => 'root-exact', 'socket_path' => '/run/codex.sock', 'client_version' => '0.160.0' }
      )
      assert_equal(manifest, runner.send(:require_ordinary_manifest_ready!, slug, manifest))
      [nil, {}, { 'state' => 'creating' }, { 'state' => 'failed' },
       { 'state' => 'ready', 'goal_sha256' => 'a' * 64, 'initial_goal_sent' => false }].each do |creation|
        invalid = manifest.merge('creation' => creation)
        assert_raises(DevSession::Error) do
          runner.send(:require_ordinary_manifest_ready!, slug, invalid)
        end
      end
      %w[repositories artifacts].each do |key|
        assert_raises(DevSession::Error) do
          runner.send(:require_ordinary_manifest_ready!, slug, manifest.reject { |name, _| name == key })
        end
      end
      %w[socket_path client_version].each do |key|
        assert_raises(DevSession::Error) do
          runner.send(:require_ordinary_manifest_ready!, slug, manifest.merge('codex' => manifest.fetch('codex').reject { |name, _| name == key }))
        end
      end
    end
  end

  def test_raw_tracking_start_archive_worktree_and_sync_refuse_before_publication
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')
      runner = runner_for(workspace)
      slug = '2026-06-06-raw'
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'active')
      root = File.join(workspace, 'work', slug)
      before = %w[plan.md state.md].to_h { |name| [name, File.binread(File.join(root, name))] }
      repository = File.join(workspace, 'repos', 'sample.git')
      refs = git_capture_success('git', "--git-dir=#{repository}", 'show-ref')
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Continue reviewed work.\n")
      actions = [
        -> { runner.start(slug, as_is: true, new: false, attach: false, run_codex: true, goal_file: goal) },
        -> { runner.archive(slug, as_is: true, abandoned: true) },
        -> { runner.worktree_add(slug, 'sample', as_is: true, name: nil, branch: nil, base: nil, fetch: false) },
        -> { runner.sync(slug, as_is: true) }
      ]
      actions.each do |action|
        error = assert_raises(DevSession::Error, &action)
        assert_includes(error.message, 'portal manifest not found')
        assert_equal(before, %w[plan.md state.md].to_h { |name| [name, File.binread(File.join(root, name))] })
        refute(File.exist?(File.join(root, 'portal.yml')))
        refute(File.exist?(File.join(workspace, 'archive', slug)))
        refute(File.exist?(File.join(workspace, 'worktrees', slug, 'sample')))
        %w[creation start archive archive-cleanup].each do |kind|
          refute(File.exist?(File.join(workspace, 'worktrees', '.locks', "#{slug}.#{kind}.json")))
        end
        assert_equal(refs, git_capture_success('git', "--git-dir=#{repository}", 'show-ref'))
      end
    end
  end

  def test_new_manifestless_revive_refuses_before_journal_or_move
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-06-06-raw-archive'
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      finalize_core(runner, slug, as_is: true)
      commit_archive_move(workspace, slug)
      configure_workspace_origin(workspace)
      root = File.join(workspace, 'archive', slug)
      before = %w[plan.md state.md].to_h { |name| [name, File.binread(File.join(root, name))] }
      error = assert_raises(DevSession::Error) { runner.revive(slug, as_is: true) }
      assert_includes(error.message, 'portal manifest not found')
      assert_equal(before, %w[plan.md state.md].to_h { |name| [name, File.binread(File.join(root, name))] })
      refute(File.exist?(File.join(workspace, 'work', slug)))
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'revive')))
      refute(File.exist?(File.join(root, 'portal.yml')))
    end
  end

  def test_creationless_start_resumes_the_exact_busy_root_and_its_start_retry
    skip 'git is not available' unless command_available?('git')

    [false, true].each do |retrying|
      with_workspace do |workspace|
        slug = '2026-06-06-retained-resume'
        runner, calls, out = ordinary_retained_runner_fixture(workspace, slug)
        File.write(File.join(workspace, 'known-busy'), 'queued input')
        if retrying
          runner.send(:write_creation_journal, runner.send(:start_journal_file, slug),
            { 'schema' => 1, 'slug' => slug, 'state' => 'creating', 'tmux_identity' => 'a' * 64 }, create: true)
        end
        before = File.binread(File.join(workspace, 'work', slug, 'portal.yml'))
        runner.start(slug, as_is: true, new: false, attach: false, run_codex: true, json: true)
        assert_equal('retained-root', JSON.parse(out.string).fetch('threadId'))
        assert_equal(before, File.binread(File.join(workspace, 'work', slug, 'portal.yml')))
        recorded = File.readlines(calls).map { |line| JSON.parse(line) }
        resume = recorded.find { |args| args[0, 2] == ['thread', 'create'] }
        assert_equal('retained-root', resume.fetch(resume.index('--thread-id') + 1))
        refute(recorded.any? { |args| args.include?('ensure-initial') })
        assert(recorded.any? { |args| args.include?('--expected-start-tmux-identity') })
        refute(File.exist?(runner.send(:start_journal_file, slug)))
        refute(File.exist?(runner.send(:creation_journal_file, slug)))
        assert_equal('ready', JSON.parse(File.read(File.join(workspace, 'authority', slug + '.json'))).fetch('state'))
        assert(retrying || recorded.first[0, 2] == ['session', 'observe'])
      end
    end
  end

  def test_creationless_start_blocks_unknown_submission_before_runtime_publication
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-retained-unknown'
      runner, calls, = ordinary_retained_runner_fixture(workspace, slug)
      File.write(File.join(workspace, 'unknown-submission'), 'unresolved')
      before = File.binread(File.join(workspace, 'work', slug, 'portal.yml'))
      error = assert_raises(DevSession::Error) do
        runner.start(slug, as_is: true, new: false, attach: false, run_codex: true)
      end
      assert_includes(error.message, 'submission readiness cannot be verified')
      assert_equal(before, File.binread(File.join(workspace, 'work', slug, 'portal.yml')))
      refute(File.exist?(runner.send(:start_journal_file, slug)))
      refute(File.exist?(File.join(workspace, 'authority', slug + '.json')))
      refute(File.readlines(calls).any? { |line| JSON.parse(line)[0, 2] == ['thread', 'create'] })
    end
  end

  def test_cold_resume_restores_exact_busy_root_without_loading_or_sending
    with_workspace do |workspace|
      slug = '2026-06-06-cold-resume'
      runner, calls, out = ordinary_retained_runner_fixture(workspace, slug)
      File.write(File.join(workspace, 'known-busy'), 'saved queued input')
      created = []
      original = runner.method(:create_tmux_session)
      runner.define_singleton_method(:create_tmux_session) do |*args, **options|
        created << options
        original.call(*args, **options)
      end
      runner.resume(slug, as_is: true)
      assert_equal(1, created.length)
      assert_equal('retained-root', created.first.fetch(:thread_id))
      assert_equal(false, created.first.fetch(:launch_codex))
      recorded = File.readlines(calls).map { |line| JSON.parse(line) }
      assert(recorded.any? { |args| args[0, 2] == ['session', 'require-recovery-ready'] })
      refute(recorded.any? { |args| args[0, 2] == ['thread', 'create'] || args.include?('ensure-initial') })
      assert_includes(out.string, 'saved work is waiting')
      authority = JSON.parse(File.read(File.join(workspace, 'authority', slug + '.json')))
      assert_equal('ready', authority.fetch('state'))
      refute(File.exist?(runner.send(:start_journal_file, slug)))
    end
  end

  def test_cold_resume_preserves_unverified_submissions_without_replaying_them
    with_workspace do |workspace|
      slug = '2026-06-06-cold-unknown'
      runner, calls, = ordinary_retained_runner_fixture(workspace, slug)
      receipt = File.join(workspace, 'unknown-submission')
      File.write(receipt, 'unresolved')
      before = File.binread(File.join(workspace, 'work', slug, 'portal.yml'))
      runner.resume(slug, as_is: true)
      assert_equal(before, File.binread(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('unresolved', File.read(receipt))
      assert(File.exist?(File.join(workspace, 'authority', slug + '.json')))
      recorded = File.readlines(calls).map { |line| JSON.parse(line) }
      refute(recorded.any? { |args| args[0, 2] == ['session', 'observe'] || args[0, 2] == ['thread', 'create'] })
    end
  end

  def test_cold_resume_refuses_changed_identity_before_terminal_creation
    with_workspace do |workspace|
      slug = '2026-06-06-cold-changed'
      runner, calls, = ordinary_retained_runner_fixture(workspace, slug)
      File.write(File.join(workspace, 'changed-identity'), 'wrong root or member')
      assert_raises(DevSession::CommandError) { runner.resume(slug, as_is: true) }
      refute(File.exist?(runner.send(:start_journal_file, slug)))
      refute(File.exist?(File.join(workspace, 'authority', slug + '.json')))
      refute(File.readlines(calls).any? { |line| JSON.parse(line)[0, 2] == ['thread', 'create'] })
    end
  end

  def test_cold_resume_reports_temporary_native_failure_without_publishing_runtime
    with_workspace do |workspace|
      slug = '2026-06-06-cold-temporary'
      runner, = ordinary_retained_runner_fixture(workspace, slug)
      File.write(File.join(workspace, 'temporary-transport'), 'socket exists but RPC is unavailable')
      error = assert_raises(DevSession::TemporaryRecoveryError) { runner.resume(slug, as_is: true) }
      assert_includes(error.message, 'temporarily unavailable')
      refute(File.exist?(runner.send(:start_journal_file, slug)))
      refute(File.exist?(File.join(workspace, 'authority', slug + '.json')))
      File.unlink(File.join(workspace, 'temporary-transport'))
      runner.resume(slug, as_is: true)
      assert(File.exist?(File.join(workspace, 'authority', slug + '.json')))
    end
  end

  def test_cold_resume_retries_lock_contention_without_changing_retained_identity
    [File::LOCK_SH, File::LOCK_EX].each do |mode|
      with_workspace do |workspace|
        slug = '2026-06-06-cold-contended'
        runner, calls, = ordinary_retained_runner_fixture(workspace, slug)
        manifest = File.join(workspace, 'work', slug, 'portal.yml')
        before = File.binread(manifest)
        lock_path = runner.send(:session_lock_file, slug)
        File.open(lock_path, File::RDWR | File::CREAT, 0o600) do |holder|
          assert(holder.flock(mode | File::LOCK_NB))
          error = assert_raises(DevSession::TemporaryRecoveryError) do
            runner.resume(slug, as_is: true)
          end
          assert_includes(error.message, 'another dev-session command is changing')
          assert_equal(before, File.binread(manifest))
          refute(File.exist?(runner.send(:start_journal_file, slug)))
          refute(File.exist?(File.join(workspace, 'authority', slug + '.json')))
          refute(File.exist?(calls), 'contention reached a native operation')
          holder.flock(File::LOCK_UN)
          runner.resume(slug, as_is: true)
        end
        authority = JSON.parse(File.read(File.join(workspace, 'authority', slug + '.json')))
        assert_equal('ready', authority.fetch('state'))
        assert_equal('retained-root', authority.fetch('codex_thread_id'))
        refute(File.exist?(runner.send(:start_journal_file, slug)))
      end
    end
  end

  def test_creationless_revive_preserves_root_scope_and_absence_of_creation
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-retained-revive'
      runner, calls, = ordinary_retained_runner_fixture(workspace, slug)
      set_lifecycle(workspace, slug, 'complete')
      assert_git_success('git', '-C', workspace, 'add', File.join('work', slug, 'state.md'))
      assert_git_success('git', '-C', workspace, 'commit', '-m', 'close initiative')
      finalize_core(runner, slug, as_is: true)
      commit_archive_move(workspace, slug)
      runner.revive(slug, as_is: true)
      manifest = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('retained-root', manifest.dig('codex', 'thread_id'))
      assert_equal([], manifest.fetch('repositories'))
      assert_equal([], manifest.fetch('artifacts'))
      refute(manifest.key?('creation'))
      recorded = File.readlines(calls).map { |line| JSON.parse(line) }
      observed = recorded.select { |args| args[0, 2] == ['session', 'observe'] }
      assert(observed.any? { |args| args.include?('--expected-revive-operation-id') })
      assert(observed.any? { |args| args.include?('--expected-revive-operation-id') && args.include?('--expected-start-tmux-identity') })
      resume = recorded.find { |args| args[0, 2] == ['thread', 'create'] }
      assert_equal('retained-root', resume.fetch(resume.index('--thread-id') + 1))
      assert_includes(resume, '--recover-archived')
      refute(recorded.any? { |args| args.include?('ensure-initial') })
      refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'revive')))
    end
  end

  def test_retained_start_contention_diagnostic_with_shared_holder
    assert_retained_start_contention_diagnostic(File::LOCK_SH, 'READ')
  end

  def test_retained_start_contention_diagnostic_with_exclusive_holder
    assert_retained_start_contention_diagnostic(File::LOCK_EX, 'WRITE')
  end

  def test_retained_start_contention_diagnostic_unlocked_retains_identity_without_output
    with_workspace do |workspace|
      slug = '2026-06-06-start-unlocked'
      runner, calls, out = ordinary_retained_runner_fixture(workspace, slug)
      before = File.binread(File.join(workspace, 'work', slug, 'portal.yml'))
      runner.start(slug, as_is: true, new: false, attach: false, run_codex: true, json: true)
      assert_equal('retained-root', JSON.parse(out.string).fetch('threadId'))
      assert_equal(before, File.binread(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('', runner.instance_variable_get(:@err).string)
      recorded = File.readlines(calls).map { |line| JSON.parse(line) }
      assert_equal(1, recorded.count { |args| args[0, 2] == ['thread', 'create'] })
      refute(recorded.any? { |args| args.include?('ensure-initial') })
      refute(File.exist?(runner.send(:start_journal_file, slug)))
    end
  end

  def test_retained_start_contention_diagnostic_rows_are_exact_bounded_and_waiters_are_not_holders
    with_workspace do |workspace|
      runner = runner_for(workspace)
      target = { 'major' => 10, 'minor' => 255, 'inode' => 12345 }
      line = "1: FLOCK ADVISORY READ 111 0a:ff:12345 0 EOF\n"
      waiter = "2: -> FLOCK ADVISORY WRITE 222 0A:FF:12345 0 EOF\n"
      unrelated = "3: FLOCK ADVISORY WRITE 999 0a:fe:12345 0 EOF\n4: FLOCK ADVISORY READ 998 0a:ff:54321 0 EOF\n"
      unknown = "5: FLOCK ADVISORY WRITE -1 0a:ff:12345 0 EOF\n"
      result = runner.send(:retained_start_lock_rows, unrelated + line + waiter + unknown, target)
      assert_equal([{ 'access' => 'READ', 'pid' => 111, 'waiter' => false },
        { 'access' => 'WRITE', 'pid' => 222, 'waiter' => true },
        { 'access' => 'WRITE', 'pid' => -1, 'waiter' => false }], result.fetch('rows'))
      refute_includes(JSON.generate(result), '999')
      limited = runner.send(:retained_start_lock_rows, line * 6, target)
      assert_equal(4, limited.fetch('rows').length)
      assert(limited.fetch('rows_truncated'))
      empty = runner.send(:retained_start_lock_rows, unrelated, target)
      assert_equal([], empty.fetch('rows'))
      assert_equal('not_observed_unknown', empty.fetch('match_status'))
      assert_equal('not_observed_unknown', runner.send(:retained_start_lock_rows, '', target).fetch('match_status'))
      assert(runner.send(:retained_start_lock_rows, "1: FLOCK malformed\n", target).fetch('malformed'))
      partial = runner.send(:retained_start_lock_rows, line + waiter.chomp, target, truncated: true)
      assert_equal([result.fetch('rows').first], partial.fetch('rows'))
    end
  end

  def test_retained_start_contention_diagnostic_reads_are_capped_and_errors_are_explicit
    with_workspace do |workspace|
      runner = runner_for(workspace)
      path = File.join(workspace, 'bounded-input')
      File.binwrite(path, 'x' * (256 * 1024 + 1))
      result = runner.send(:retained_start_diagnostic_read, path, 256 * 1024)
      assert_equal(256 * 1024, result.fetch('data').bytesize)
      assert(result.fetch('truncated'))
      assert_equal('exited_or_unavailable', runner.send(:retained_start_diagnostic_read, path + '-missing', 128).fetch('status'))
      assert_equal('denied', runner.send(:retained_start_unavailable, Errno::EACCES.new))
      assert_equal('denied', runner.send(:retained_start_unavailable, Errno::EPERM.new))
      assert_equal('unavailable', runner.send(:retained_start_unavailable, IOError.new))
      assert_equal('exited_or_unavailable', runner.send(:retained_start_unavailable, Errno::ESRCH.new))
    end
  end

  def test_retained_start_contention_diagnostic_actor_identity_can_be_unstable_exited_or_truncated
    with_workspace do |workspace|
      runner = runner_for(workspace)
      before = { 'status' => 'ok', 'start_time' => '100' }
      after = { 'status' => 'ok', 'start_time' => '101' }
      samples = [before, after]
      runner.define_singleton_method(:retained_start_process_time) { |_pid| samples.shift }
      actor = runner.send(:retained_start_holder_sample, Process.pid)
      assert_equal('unstable', actor.fetch('status'))
      assert_equal(before, actor.fetch('start_before'))
      assert_equal(after, actor.fetch('start_after'))
      runner.define_singleton_method(:retained_start_process_time) { |_pid| { 'status' => 'exited_or_unavailable' } }
      assert_equal('unknown_or_exited', runner.send(:retained_start_holder_sample, Process.pid).fetch('status'))
      runner.define_singleton_method(:retained_start_diagnostic_read) { |_path, _limit| { 'status' => 'ok', 'data' => 'cut', 'truncated' => true } }
      # Use the original time parser on the truncated bounded input.
      time = DevSession::Runner.instance_method(:retained_start_process_time).bind_call(runner, Process.pid)
      assert_equal({ 'status' => 'truncated', 'truncated' => true }, time)
      runner.define_singleton_method(:retained_start_diagnostic_read) { |_path, _limit| { 'status' => 'denied', 'truncated' => false } }
      time = DevSession::Runner.instance_method(:retained_start_process_time).bind_call(runner, Process.pid)
      assert_equal('denied', time.fetch('status'))
      runner.define_singleton_method(:retained_start_diagnostic_read) { |_path, _limit| { 'status' => 'ok', 'data' => 'malformed', 'truncated' => false } }
      assert_equal('unknown', DevSession::Runner.instance_method(:retained_start_process_time).bind_call(runner, Process.pid).fetch('status'))
    end
  end

  def test_retained_start_contention_diagnostic_uses_open_fd_and_only_sampled_positive_holders
    with_workspace do |workspace|
      slug = '2026-06-06-start-descriptor'
      runner, calls, = ordinary_retained_runner_fixture(workspace, slug)
      path = runner.send(:session_lock_file, slug)
      observed = nil
      emitter = runner.method(:emit_retained_start_contention)
      runner.define_singleton_method(:emit_retained_start_contention) do |file, target|
        observed = { fd: file.fileno, inode: file.stat.ino, mode: file.stat.mode }
        emitter.call(file, target)
        raise 'descriptor unexpectedly closed' if file.closed?
      end
      with_retained_start_holder(path, File::LOCK_EX) do |pid|
        assert_raises(DevSession::SessionBusyError) { runner.start(slug, as_is: true, new: false, attach: false, run_codex: true) }
        diagnostic = JSON.parse(runner.instance_variable_get(:@err).string.split(': ', 2).last)
        assert_equal(observed.fetch(:fd), diagnostic.fetch('fd'))
        assert_equal(observed.fetch(:inode), diagnostic.fetch('target').fetch('inode'))
        assert_equal(0o600, observed.fetch(:mode) & 0o777)
        if diagnostic.fetch('locks').fetch('status') == 'ok'
          assert_equal([pid], diagnostic.fetch('holders').map { |actor| actor.fetch('pid') })
        end
        refute(File.exist?(calls))
      end
      File.open(path, File::RDWR) do |file|
        stat = file.stat
        reads, actors = [], []
        runner.define_singleton_method(:retained_start_diagnostic_read) do |requested, limit|
          reads << [requested, limit]
          rows = "1: -> FLOCK ADVISORY WRITE 123 #{stat.dev_major.to_s(16)}:#{stat.dev_minor.to_s(16)}:#{stat.ino} 0 EOF\n"
          rows += "2: FLOCK ADVISORY READ -1 #{stat.dev_major.to_s(16)}:#{stat.dev_minor.to_s(16)}:#{stat.ino} 0 EOF\n"
          { 'status' => 'ok', 'data' => rows, 'truncated' => false }
        end
        runner.define_singleton_method(:retained_start_holder_sample) { |pid| actors << pid; {} }
        runner.instance_variable_set(:@err, StringIO.new)
        # A mismatched pathname is evidence only; it does not replace the FD identity.
        emitter.call(file, path + '-missing')
        assert_equal([['/proc/locks', 256 * 1024]], reads)
        assert_equal([], actors)
        record = JSON.parse(runner.instance_variable_get(:@err).string.split(': ', 2).last)
        assert_equal('unavailable', record.fetch('path_status'))
        assert_equal('not_observed_unknown', runner.send(:retained_start_lock_rows, '', record.fetch('target')).fetch('match_status'))
      end
    end
  end

  def test_retained_start_contention_diagnostic_failures_preserve_real_refusal_and_cli_classification
    %w[read parse serialize sink emitter].each do |failure|
      with_workspace do |workspace|
        slug = '2026-06-06-start-failure'
        runner, calls, out = ordinary_retained_runner_fixture(workspace, slug)
        path = runner.send(:session_lock_file, slug)
        errors = StringIO.new
        if failure == 'sink'
          errors.define_singleton_method(:write) do |text|
            raise IOError, 'diagnostic sink unavailable' if text.start_with?('dev-session retained-start contention: ')
            super(text)
          end
        elsif failure == 'read'
          runner.define_singleton_method(:retained_start_diagnostic_read) { |*_args| raise IOError, 'diagnostic read failed' }
        elsif failure == 'parse'
          runner.define_singleton_method(:retained_start_lock_rows) { |*_args, **_options| raise ArgumentError, 'diagnostic parse failed' }
        elsif failure == 'serialize'
          runner.define_singleton_method(:retained_start_lock_rows) { |*_args, **_options| { 'rows' => [], 'nonfinite' => Float::NAN } }
        else
          runner.define_singleton_method(:emit_retained_start_contention) { |*_args| raise IOError, 'emitter failed' }
        end
        runner.instance_variable_set(:@err, errors)
        manifest = File.join(workspace, 'work', slug, 'portal.yml')
        before = File.binread(manifest)
        with_retained_start_holder(path, File::LOCK_EX) do
          cli = DevSession::CLI.new(['start', slug, '--as-is', '--no-attach', '--json'], out:, err: errors, env: {})
          cli.define_singleton_method(:runner) { runner }
          assert_equal(1, cli.run, failure)
          assert_equal('', out.string, failure)
          assert_equal("error: another dev-session command is changing #{slug}\n", errors.string, failure)
          assert_equal(before, File.binread(manifest))
          refute(File.exist?(calls))
          refute(File.exist?(runner.send(:start_journal_file, slug)))
          refute(File.exist?(File.join(workspace, 'authority', slug + '.json')))
          File.open(runner.send(:session_lock_file, slug, kind: 'creation'), File::RDWR) do |outer|
            assert(outer.flock(File::LOCK_EX | File::LOCK_NB), "creation lock leaked after #{failure}")
          end
        end
      end
    end
  end

  def test_retained_start_contention_diagnostic_unavailable_empty_and_truncated_samples_stay_unknown
    with_workspace do |workspace|
      runner = runner_for(workspace)
      path = File.join(workspace, 'sample.lock')
      File.open(path, File::RDWR | File::CREAT, 0o600) do |file|
        %w[denied exited_or_unavailable ok].each do |status|
          runner.define_singleton_method(:retained_start_diagnostic_read) do |_path, _limit|
            { 'status' => status, 'data' => '', 'truncated' => status == 'ok' }
          end
          output = StringIO.new
          runner.instance_variable_set(:@err, output)
          runner.send(:emit_retained_start_contention, file, path)
          record = JSON.parse(output.string.split(': ', 2).last)
          assert_equal(status, record.fetch('locks').fetch('status'))
          assert_equal([], record.fetch('locks').fetch('rows'))
          assert_equal('not_observed_unknown', record.fetch('locks').fetch('match_status'))
          assert_equal([], record.fetch('holders'))
          assert_operator(output.string.bytesize, :<=, 8192)
        end
        other = File.join(workspace, 'different.lock')
        File.write(other, '')
        output = StringIO.new
        runner.instance_variable_set(:@err, output)
        runner.send(:emit_retained_start_contention, file, other)
        assert_equal('mismatch', JSON.parse(output.string.split(': ', 2).last).fetch('path_status'))
        # Best-effort serialization never emits an oversized record.
        runner.send(:emit_retained_start_contention, file, 'x' * 8192)
        assert_equal(1, output.string.lines.length)
      end
    end
  end

  private
  def with_retained_start_holder(path, mode)
    ready_read, ready_write = IO.pipe
    release_read, release_write = IO.pipe
    pid = fork do
      ready_read.close
      release_write.close
      begin
        File.open(path, File::RDWR | File::CREAT, 0o600) do |file|
          raise 'fixture lock unavailable' unless file.flock(mode | File::LOCK_NB)
          ready_write.write('1')
          ready_write.close
          release_read.read(1)
        end
        exit! 0
      rescue StandardError
        exit! 2
      end
    end
    ready_write.close
    release_read.close
    assert(IO.select([ready_read], nil, nil, 3), 'holder readiness timed out')
    assert_equal('1', ready_read.read(1), 'holder exited before locking')
    yield pid
  ensure
    [ready_read, ready_write, release_read, release_write].compact.each { |io| io.close unless io.closed? }
    if pid
      _, status = Process.wait2(pid)
      assert(status.success?, 'holder did not join successfully')
    end
  end

  def assert_retained_start_contention_diagnostic(mode, access)
    with_workspace do |workspace|
      slug = '2026-06-06-start-contended'
      runner, calls, out = ordinary_retained_runner_fixture(workspace, slug)
      err = runner.instance_variable_get(:@err)
      manifest = File.join(workspace, 'work', slug, 'portal.yml')
      before = File.binread(manifest)
      path = runner.send(:session_lock_file, slug)
      with_retained_start_holder(path, mode) do |pid|
        error = assert_raises(DevSession::SessionBusyError) do
          runner.start(slug, as_is: true, new: false, attach: false, run_codex: true, json: true)
        end
        assert_equal("another dev-session command is changing #{slug}", error.message)
        assert_equal('', out.string)
        assert_equal(before, File.binread(manifest))
        refute(File.exist?(runner.send(:start_journal_file, slug)))
        refute(File.exist?(runner.send(:creation_journal_file, slug)))
        refute(File.exist?(File.join(workspace, 'authority', slug + '.json')))
        refute(File.exist?(calls), 'contention reached a native operation')
        File.open(runner.send(:session_lock_file, slug, kind: 'creation'), File::RDWR) do |outer|
          assert(outer.flock(File::LOCK_EX | File::LOCK_NB), 'creation lock leaked after refusal')
        end
        prefix = 'dev-session retained-start contention: '
        lines = err.string.lines
        assert_equal(1, lines.length, 'expected bounded contention diagnostic after real busy refusal')
        assert(lines.first.start_with?(prefix), 'missing fixed diagnostic prefix')
        assert_operator(err.string.bytesize, :<=, 8192)
        diagnostic = JSON.parse(lines.first.delete_prefix(prefix))
        stat = File.stat(path)
        assert_equal(Process.pid, diagnostic.fetch('pid'))
        assert_operator(diagnostic.fetch('fd'), :>=, 0)
        assert_equal({ 'major' => stat.dev_major, 'minor' => stat.dev_minor, 'inode' => stat.ino }, diagnostic.fetch('target'))
        assert_equal(path, diagnostic.fetch('path'))
        assert_equal('EX|NB', diagnostic.fetch('requested'))
        assert_equal('same', diagnostic.fetch('path_status'))
        assert_operator(diagnostic.fetch('sample_end'), :>=, diagnostic.fetch('sample_start'))
        if diagnostic.fetch('locks').fetch('status') == 'ok'
          row = diagnostic.fetch('locks').fetch('rows').find { |item| item['pid'] == pid && !item['waiter'] }
          refute_nil(row, 'known live holder missing from procfs sample')
          assert_equal(access, row.fetch('access'))
          assert_equal('stable', diagnostic.fetch('holders').find { |item| item['pid'] == pid }.fetch('status'))
        end
      end
    end
  end


  # Ruby orchestration fixture; native identity/activity protocols are covered
  # by the existing Go owners. It writes no native rollout or submission ledger.
  def ordinary_retained_runner_fixture(workspace, slug)
    portal, calls = File.join(workspace, 'ordinary-portal.rb'), File.join(workspace, 'ordinary-calls.jsonl')
    File.write(portal, <<~'RUBY')
      require 'digest'
      require 'json'
      require 'yaml'
      File.open(File.join(File.dirname(__FILE__), 'ordinary-calls.jsonl'), 'a') { |file| file.puts(JSON.generate(ARGV)) }
      if ARGV[0, 2] == ['thread', 'create']
        abort 'unexpected fresh root' unless ARGV.include?('--thread-id') && ARGV[ARGV.index('--thread-id') + 1] == 'retained-root'
        puts JSON.generate(threadId: 'retained-root')
      elsif ARGV[0, 2] == ['session', 'require-recovery-ready']
        workspace = ARGV.fetch(ARGV.index('--workspace') + 1)
        exit 75 if File.exist?(File.join(workspace, 'temporary-transport'))
        abort 'changed retained identity' if File.exist?(File.join(workspace, 'changed-identity'))
      elsif ARGV[0, 2] == ['session', 'observe']
        workspace = ARGV.fetch(ARGV.index('--workspace') + 1)
        slug = ARGV.fetch(ARGV.index('--session-slug') + 1)
        root = File.join(workspace, 'work', slug)
        manifest = YAML.safe_load(File.read(File.join(root, 'portal.yml')))
        thread = manifest.dig('codex', 'thread_id')
        stat = File.lstat(root)
        identity = Digest::SHA256.hexdigest(JSON.generate(['session-observation-v1', workspace, slug, stat.dev, stat.ino, thread]))
        temporary = File.exist?(File.join(workspace, 'temporary-transport'))
        known = !temporary && !File.exist?(File.join(workspace, 'unknown-submission'))
        idle = known && !File.exist?(File.join(workspace, 'known-busy'))
        token = Digest::SHA256.hexdigest('actual test activity')
        diagnostics = known ? (idle ? [] : [{ 'code' => 'queued_input', 'category' => 'busy', 'message' => 'Queued input.' }]) :
          [{ 'code' => temporary ? 'transport_unavailable' : 'submission_unverified', 'category' => 'activity_unknown', 'message' => 'Submission proof unavailable.' }]
        subjects = known ? [{ 'address' => 'lead', 'threadId' => thread, 'activityToken' => token, 'lastActivityAt' => nil, 'idle' => idle, 'archiveState' => 'active' }] : []
        puts JSON.generate('schema' => 1, 'workspace' => workspace, 'slug' => slug, 'identity' => identity, 'activityKnown' => known,
          'activityToken' => known ? token : '', 'lastActivityAt' => nil, 'idle' => idle, 'subjects' => subjects, 'diagnostics' => diagnostics)
      end
    RUBY
    session = DevSession::Tmux::Session.new(id: '$1', name: slug, mark: '1', slug:, workspace:, environment_slug: slug,
      socket_path: '/run/test/tmux.sock', codex_thread_id: 'retained-root', codex_socket_path: '/run/test/codex.sock',
      codex_client_version: '0.160.0', codex_pane_id: '%1')
    runner_class = Class.new(DevSession::Runner) do
      define_method(:create_tmux_session) do |*_args, **options|
        session.identity_token = options.fetch(:identity_token)
        session
      end
      define_method(:sync_slug) { |*_args, **_options| session }
      define_method(:revalidate_session!) { |selected| selected }
      define_method(:verify_codex_client!) {}
    end
    out = StringIO.new
    runner = runner_class.new(workspace:, tmux: NullTmux.new, out:, err: StringIO.new, today: TODAY,
      env: { DevSession::ENV_PORTAL_COMMAND => [RbConfig.ruby, portal].shelljoin,
        DevSession::ENV_CODEX_SOCKET => '/run/test/codex.sock', DevSession::ENV_CODEX_VERSION => '0.160.0',
        DevSession::ENV_AUTHORITY_DIR => File.join(workspace, 'authority'), 'DEV_WORKSPACE_CODEX_HOME' => File.join(workspace, 'codex') })
    runner.ensure_tracking_files(slug)
    runner.send(:write_portal_manifest, slug, runner.send(:new_threadless_portal_manifest, slug).merge(
      'codex' => { 'thread_id' => 'retained-root', 'socket_path' => '/run/test/codex.sock', 'client_version' => '0.160.0' }))
    commit_tracking(workspace, slug, lifecycle: 'active')
    configure_workspace_origin(workspace)
    [runner, calls, out]
  end
end
