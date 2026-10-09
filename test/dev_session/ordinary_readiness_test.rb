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
      assert(recorded.any? { |args| args[0, 2] == ['session', 'observe'] })
      refute(recorded.any? { |args| args[0, 2] == ['thread', 'create'] || args.include?('ensure-initial') })
      assert_includes(out.string, 'saved work is waiting')
      authority = JSON.parse(File.read(File.join(workspace, 'authority', slug + '.json')))
      assert_equal('ready', authority.fetch('state'))
      refute(File.exist?(runner.send(:start_journal_file, slug)))
    end
  end

  def test_cold_resume_refuses_unverified_submissions_before_terminal_creation
    with_workspace do |workspace|
      slug = '2026-06-06-cold-unknown'
      runner, calls, = ordinary_retained_runner_fixture(workspace, slug)
      File.write(File.join(workspace, 'unknown-submission'), 'unresolved')
      before = File.binread(File.join(workspace, 'work', slug, 'portal.yml'))
      assert_raises(DevSession::Error) { runner.resume(slug, as_is: true) }
      assert_equal(before, File.binread(File.join(workspace, 'work', slug, 'portal.yml')))
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

  private

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
