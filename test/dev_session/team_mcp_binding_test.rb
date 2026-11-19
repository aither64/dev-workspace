# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class TeamMcpBindingTest < DevSessionTest
  class RecordingCapture
    attr_reader :calls

    def initialize
      @calls = []
    end

    def capture(argv, input: nil)
      @calls << [argv, input]
      ['', '', nil]
    end
  end

  class RecordingTeamRunner
    attr_reader :calls

    def initialize
      @calls = []
    end

    def resolve_slug(value, as_is:)
      value
    end

    def team(slug, action:, **options)
      @calls << [slug, action, options]
      ''
    end
  end

  def test_report_refuses_recreated_slug_with_a_different_lead_thread
    with_workspace do |workspace|
      slug = 'member-report'
      original_root = 'original-lead-thread'
      current_root = 'replacement-lead-thread'
      runner = team_runner(workspace, slug, original_root)
      prepare_team_manifest(runner, slug, current_root)

      error = assert_raises(DevSession::Error) do
        runner.team(slug, action: 'assign', from: 'reviewer0', to: 'lead',
                    message: 'Report text', message_id: 'a' * 32)
      end
      assert_includes(error.message, 'lead thread changed')

      current = team_runner(workspace, slug, current_root)
      assert_equal('', current.team(slug, action: 'assign', from: 'reviewer0', to: 'lead',
                                    message: 'Report text', message_id: 'a' * 32))
    end
  end

  def test_assign_message_uses_stdin_across_both_cli_hops
    with_workspace do |workspace|
      slug = 'member-report'
      runner = team_runner(workspace, slug, 'lead-thread')
      prepare_team_manifest(runner, slug, 'lead-thread')

      capture = RecordingCapture.new
      reporter = team_runner(workspace, slug, 'lead-thread', command_runner: capture)
      assert_equal('', reporter.team(slug, action: 'assign', from: 'reviewer0', to: 'lead',
                                     message: 'Private report text', message_id: 'b' * 32))
      assert_equal(1, capture.calls.length)
      argv, input = capture.calls.fetch(0)
      assert_equal('Private report text', input)
      assert_equal('/dev/stdin', argv.fetch(argv.index('--input-file') + 1))
      assert_equal(File.dirname(File.dirname(File.realpath(RbConfig.ruby))),
                   argv.fetch(argv.index('--package-root') + 1))
      assert_equal('actual-member-thread', argv.fetch(argv.index('--expected-member-thread-id') + 1))
      assert_equal(File.join(workspace, '.xdg-state', 'dev-workspaces', 'profile'),
                   argv.fetch(argv.index('--host-profile') + 1))
      refute_includes(argv.join(' '), 'Private report text')
      refute_includes(argv, '--message')
    end
  end

  def test_message_stdin_is_bounded_and_exclusive_to_assign
    recorder = RecordingTeamRunner.new
    command = ['team', 'assign', 'member-report', '--as-is', '--to', 'lead',
               '--message-stdin', '--message-id', 'c' * 32]
    output = StringIO.new
    errors = StringIO.new
    cli = DevSession::CLI.new(command, input: StringIO.new('Private report text'), out: output, err: errors)
    cli.instance_variable_set(:@runner, recorder)
    assert_equal(0, cli.run)
    assert_equal('Private report text', recorder.calls.fetch(0).fetch(2).fetch(:message))

    conflicting = DevSession::CLI.new(command + ['--message', 'other'], input: StringIO.new('secret'),
                                      out: StringIO.new, err: StringIO.new)
    conflicting.instance_variable_set(:@runner, recorder)
    assert_equal(1, conflicting.run)
    oversized = DevSession::CLI.new(command, input: StringIO.new('x' * (DevSession::MAX_MESSAGE_BYTES + 1)),
                                    out: StringIO.new, err: StringIO.new)
    oversized.instance_variable_set(:@runner, recorder)
    assert_equal(1, oversized.run)
    assert_equal(1, recorder.calls.length)
  end

  def test_pi_team_uses_registered_portal_without_codex_thread_or_command_lock
    with_workspace do |workspace|
      slug = '2026-11-18-pi-team'
      capture = RecordingCapture.new
      instance = File.join(workspace, 'registered-instance')
      runner = DevSession::Runner.new(workspace:, portal_command: [RbConfig.ruby, '-e', ''],
        codex_socket: File.join(instance, 'app-server.sock'), authority_dir: File.join(instance, 'authority'),
        portal_url: 'https://portal.example', command_runner: capture,
        out: StringIO.new, err: StringIO.new, env: {'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')})
      runner.ensure_tracking_files(slug)
      manifest = {'schema' => 4, 'slug' => slug,
        'backend' => {'kind' => 'pi', 'contract' => 1, 'session_incarnation' => 'a' * 64},
        'creation' => {'state' => 'creating', 'goal_sha256' => 'b' * 64, 'initial_goal_sent' => false},
        'repositories' => []}
      runner.send(:write_portal_manifest, slug, manifest)
      runner.define_singleton_method(:with_slug_lock) { |_slug, &_block| raise 'Codex command lock entered' }
      assert_equal('', runner.team(slug, action: 'assign', from: 'lead', to: 'reviewer0', message: 'Private task', message_id: 'a' * 32))
      argv, input = capture.calls.fetch(0)
      assert_equal('Private task', input)
      assert_equal(File.join(instance, 'portal.sock'), argv.fetch(argv.index('--portal-socket') + 1))
      refute_includes(argv, '--root-thread-id')
      refute_includes(argv.join(' '), 'Private task')
      assert_raises(DevSession::Error) { runner.team(slug, action: 'assign', to: 'reviewer0', message: 'Task', message_id: 'a' * 32, model: 'ignored') }
      assert_equal(1, capture.calls.length)
      assert_equal('', runner.team(slug, action: 'remove', address: 'reviewer0'))
      remove, input = capture.calls.fetch(1)
      assert_includes(remove, 'remove')
      assert_equal('reviewer0', remove.fetch(remove.index('--address') + 1))
      assert_equal(File.join(instance, 'portal.sock'), remove.fetch(remove.index('--portal-socket') + 1))
      refute_includes(remove, '--root-thread-id')
      assert_nil(input)
      assert_raises(DevSession::Error) { runner.team(slug, action: 'remove', address: 'reviewer0', model: 'ignored') }
      assert_equal(2, capture.calls.length)
    end
  end

  def test_pi_fork_uses_serving_portal_selected_member_without_legacy_locks
    with_workspace do |workspace|
      root = File.join(workspace, '.test-user-state')
      instance = File.join(workspace, 'registered-instance')
      capture = RecordingCapture.new
      tmux = Object.new
      tmux.define_singleton_method(:managed_session_identities) { [] }
      runner = DevSession::Runner.new(workspace:, tmux:, portal_command: [RbConfig.ruby, '-e', ''],
        codex_socket: File.join(instance, 'app-server.sock'), authority_dir: File.join(instance, 'authority'),
        portal_url: 'https://portal.example', command_runner: capture, out: StringIO.new, err: StringIO.new,
        env: {'DEV_WORKSPACES_STATE' => root})
      slug = '2026-11-18-pi-source'
      runner.ensure_tracking_files(slug)
      directory = WorkspaceSessionBackends.binding_directory(root, workspace)
      FileUtils.mkdir_p(directory, mode: 0700)
      binding = {'schema' => 1, 'backend' => 'pi', 'backend_contract' => 1, 'workspace' => workspace, 'slug' => slug,
        'session_incarnation' => 'a' * 64, 'creation_receipt_id' => 'b' * 64, 'revision' => 2, 'state' => 'ready',
        'conversations' => [{'address' => 'lead', 'incarnation' => 'c' * 64, 'state' => 'ready', 'native_id' => 'native-lead'},
          {'address' => 'reviewer0', 'incarnation' => 'd' * 64, 'state' => 'ready', 'native_id' => 'native-reviewer'}]}
      File.write(File.join(directory, "#{slug}.json"), JSON.generate(binding), mode: 'w', perm: 0600)
      runner.define_singleton_method(:with_creation_lock) { |*| raise 'legacy creation entered' }
      runner.fork(slug, '2026-11-18-pi-fork', as_is: true, json: true, member: 'reviewer0')
      argv, = capture.calls.fetch(0)
      assert_includes(argv, 'fork')
      assert_equal('reviewer0', argv.fetch(argv.index('--member') + 1))
      assert_equal(File.join(instance, 'portal.sock'), argv.fetch(argv.index('--portal-socket') + 1))
      %w[--model --effort --provider --goal-file --root-thread-id].each { |key| refute_includes(argv, key) }
      assert_raises(DevSession::Error) { runner.fork(slug, '2026-11-18-pi-fork', as_is: true, json: true, model: 'ignored') }
      assert_equal(1, capture.calls.length)
      # A backend-record error is not a source lookup failure and must never
      # enter legacy destination locks or select the Codex path.
      File.write(File.join(directory, "#{slug}.json"), '{}', mode: 'w', perm: 0600)
      assert_raises(WorkspaceSessionBackends::Error) do
        runner.fork(slug, '2026-11-18-pi-fork', as_is: true, json: true, member: 'reviewer0')
      end
      assert_equal(1, capture.calls.length)
    end
  end

  private

  def team_runner(workspace, slug, expected_root, command_runner: nil)
    DevSession::Runner.new(workspace:, tmux: Object.new, portal_command: [RbConfig.ruby, '-e', ''],
                           command_runner:, out: StringIO.new, err: StringIO.new, env: {
      'XDG_STATE_HOME' => File.join(workspace, '.xdg-state'),
      DevSession::ENV_SLUG => slug,
      DevSession::ENV_MEMBER_ADDRESS => 'reviewer0',
      DevSession::ENV_EXPECTED_ROOT_THREAD_ID => expected_root,
      DevSession::ENV_EXPECTED_MEMBER_THREAD_ID => 'actual-member-thread'
    })
  end

  def prepare_team_manifest(runner, slug, root_thread)
    runner.ensure_tracking_files(slug)
    manifest = runner.send(:new_portal_manifest, slug)
    manifest['codex']['thread_id'] = root_thread
    runner.send(:write_portal_manifest, slug, manifest)
  end
end
