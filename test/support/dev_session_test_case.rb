# frozen_string_literal: true

require 'date'
require 'fileutils'
require 'json'
require 'minitest/autorun'
require 'open3'
require 'pty'
require 'rbconfig'
require 'shellwords'
require 'stringio'
require 'tmpdir'

load File.expand_path('../../libexec/dev-session', __dir__)

class DevSessionTest < Minitest::Test

  private

  def automatic_fixture(workspace, slug, lifecycle: 'active', out: nil)
    portal = File.join(workspace, 'automatic-portal.rb')
    stamp = Time.now.to_i
    File.write(portal, <<~RUBY)
      require 'json'
      exit 0 unless ARGV.first == 'thread'
      cwd = ARGV.fetch(ARGV.index('--cwd') + 1)
      if ARGV[1] == 'activity'
        puts JSON.generate('threadId' => ARGV.fetch(ARGV.index('--thread-id') + 1),
                           'cwd' => cwd, 'updatedAt' => #{stamp})
      elsif ARGV[1] == 'require-idle' && File.exist?(File.join(cwd, 'busy'))
        warn 'Conversation has a queued message.'
        exit 1
      end
    RUBY
    runner = runner_for(workspace, out:, env: {
      DevSession::ENV_PORTAL_COMMAND => [RbConfig.ruby, portal].shelljoin,
      DevSession::ENV_CODEX_SOCKET => '/run/test/codex.sock',
      DevSession::ENV_CODEX_VERSION => '0.152.1'
    })
    runner.ensure_tracking_files(slug)
    runner.send(:ensure_portal_manifest, slug)
    path = File.join(workspace, 'work', slug, 'portal.yml')
    manifest = YAML.safe_load(File.read(path))
    manifest['codex'] = {
      'thread_id' => "thread-#{slug}", 'socket_path' => '/run/test/codex.sock', 'client_version' => '0.152.1'
    }
    File.write(path, YAML.dump(manifest))
    commit_tracking(workspace, slug, lifecycle:)
    configure_workspace_origin(workspace)
    runner.auto_archive_configure(true)
    runner
  end

  def automatic_scan(runner, dry_run: false)
    runner.auto_archive_scan(dry_run:, transition: ->(&block) { block.call })
  end

  def age_automatic_session(runner, slug, seconds)
    store = runner.auto_archive_store
    store.lock do
      record = store.session(slug)
      record['idle_since'] = (Time.now - seconds).utc.iso8601
      store.write("session-#{slug}", record)
    end
  end

  def portal_creation_expectation(workspace, source_slug, destination:, id: 'a' * 64)
    directory = File.join(workspace, '.portal-private')
    FileUtils.mkdir_p(directory, mode: 0o700)
    write_portal_creation_acceptance(workspace, destination, id: id, directory: directory)
    source = File.join(workspace, 'work', source_slug)
    manifest = YAML.safe_load(File.read(File.join(source, 'portal.yml')))
    stat = File.lstat(source)
    identity = Digest::SHA256.hexdigest([
      source, stat.dev, stat.ino, stat.ctime.to_i, stat.ctime.nsec,
      manifest.dig('codex', 'thread_id')
    ].join("\0"))
    {
      creation_receipt_id: id, creation_evidence: File.join(directory, "#{id}.complete.json"),
      expected_source: source_slug, expected_source_thread: manifest.dig('codex', 'thread_id'),
      expected_source_identity: identity
    }
  end

  def write_portal_creation_acceptance(workspace, slug, id:, directory:)
    # Only the receipt identity and history are inputs to the Ruby boundary;
    # Go tests own the full accepted request serialization.
    receipt = {
      'schema' => 1, 'workspace' => workspace, 'receiptId' => id,
      'request' => { 'slug' => slug },
      'deletionHistorySha256' => runner_for(workspace).send(:completed_removal_history, slug)
    }
    path = File.join(directory, "#{slug}.json")
    File.write(path, JSON.generate(receipt))
    File.chmod(0o600, path)
    path
  end

  def previous_tracking_template(kind, slug)
    File.read(File.join(__dir__, '..', 'fixtures', "session-#{kind}-before-documentation.md"))
      .sub('{{slug}}', slug)
  end

  def removal_recovery(workspace, slug)
    matches = Dir.glob(
      File.join(workspace, '.xdg-state', 'dev-workspaces', 'removed', '*', "*-#{slug}-*")
    )
    assert_equal(1, matches.length, "expected one recovery directory for #{slug}")
    matches.fetch(0)
  end

  def with_workspace
    Dir.mktmpdir('dev-session-test') do |workspace|
      FileUtils.mkdir_p(File.join(workspace, 'repos'))
      FileUtils.mkdir_p(File.join(workspace, 'work'))
      FileUtils.mkdir_p(File.join(workspace, 'worktrees'))
      yield workspace
    end
  end

  def profile_link_token(path)
    DevWorkspaceProfileIdentity.token(path)
  end

  def cleanup_contract_helper(workspace, name, paths)
    helper = File.join(workspace, "#{name}-cleanup-contract")
    payload = JSON.generate('schema' => 1, 'paths' => paths)
    File.write(helper, <<~SH)
      #!/bin/sh
      [ "$1" = cleanup-paths ] || exit 0
      printf '%s\n' #{Shellwords.escape(payload)}
    SH
    File.chmod(0o755, helper)
    helper
  end

  def runner_for(
    workspace,
    env: {},
    cwd: nil,
    tmux: nil,
    out: nil,
    authority_dir: nil,
    alpha_cluster: nil,
    beta_cluster: nil
  )
    out ||= StringIO.new
    tmux ||= NullTmux.new
    resolved_env = {
      'XDG_STATE_HOME' => File.join(workspace, '.xdg-state')
    }.merge(env)
    cluster_providers = { }
    cluster_providers['alpha'] = alpha_cluster if alpha_cluster
    cluster_providers['beta'] = beta_cluster if beta_cluster

    DevSession::Runner.new(
      workspace:,
      authority_dir:,
      tmux:,
      out:,
      err: StringIO.new,
      today: TODAY,
      env: resolved_env,
      cwd: cwd || workspace,
      cluster_providers:
    )
  end

  def archived_runner(workspace, slug)
    runner = runner_for(workspace)
    runner.ensure_tracking_files(slug)
    commit_tracking(workspace, slug, lifecycle: 'complete')
    finalize_core(runner, slug, as_is: true)
    commit_archive_move(workspace, slug)
    runner
  end

  def finalize_core(runner, input, as_is:)
    slug = runner.send(:lookup_slug, input, as_is:)
    runner.send(:select_tmux_for_slug!, slug)
    runner.send(:finalize_locked!, slug)
  end

  def create_bare_repo(workspace, project)
    source = File.join(workspace, "source-#{project}")
    bare = File.join(workspace, 'repos', "#{project}.git")

    assert_git_success('git', 'init', '-b', 'master', source)
    assert_git_success('git', '-C', source, 'config', 'user.email', 'test@example.invalid')
    assert_git_success('git', '-C', source, 'config', 'user.name', 'Test User')
    assert_git_success('git', '-C', source, 'config', 'receive.denyCurrentBranch', 'updateInstead')
    File.write(File.join(source, 'README.md'), "# Test\n")
    assert_git_success('git', '-C', source, 'add', 'README.md')
    assert_git_success('git', '-C', source, 'commit', '-m', 'initial')
    assert_git_success('git', 'clone', '--bare', source, bare)
  end

  def commit_tracking(workspace, slug, lifecycle:)
    assert_git_success('git', 'init', '-b', 'master', workspace)
    assert_git_success('git', '-C', workspace, 'config', 'user.email', 'test@example.invalid')
    assert_git_success('git', '-C', workspace, 'config', 'user.name', 'Test User')
    assert_git_success('git', '-C', workspace, 'add', File.join('work', slug))
    assert_git_success('git', '-C', workspace, 'commit', '-m', 'start initiative')
    return if lifecycle == 'active'

    set_lifecycle(workspace, slug, lifecycle)
    assert_git_success('git', '-C', workspace, 'add', File.join('work', slug, 'state.md'))
    assert_git_success('git', '-C', workspace, 'commit', '-m', 'close initiative')
  end

  def configure_workspace_origin(workspace)
    remote = File.join(workspace, '.git', 'test-origin.git')
    assert_git_success('git', 'init', '--bare', remote)
    assert_git_success('git', '-C', workspace, 'remote', 'add', 'origin', remote)
    assert_git_success('git', '-C', workspace, 'push', '-u', 'origin', 'master')
  end

  def merge_registered_branches(workspace, slug)
    path = File.join(workspace, 'work', slug, 'portal.yml')
    return unless File.file?(path)

    manifest = YAML.safe_load(File.read(path))
    manifest.fetch('repositories', []).each do |repository|
      common = if repository.fetch('project') == 'workspace'
                 File.join(workspace, '.git')
               else
                 File.join(workspace, 'repos', "#{repository.fetch('project')}.git")
               end
      branch = repository.fetch('branch')
      default = repository.fetch('default_branch')
      assert_git_success(
        'git', "--git-dir=#{common}", 'push', 'origin',
        "refs/heads/#{branch}:refs/heads/#{branch}"
      )
      assert_git_success(
        'git', "--git-dir=#{common}", 'push', 'origin',
        "refs/heads/#{branch}:refs/heads/#{default}"
      )
    end
  end

  def commit_terminal_tracking_only(workspace, slug, lifecycle:)
    set_lifecycle(workspace, slug, lifecycle)
    assert_git_success('git', 'init', '-b', 'master', workspace)
    configure_git_identity(workspace)
    assert_git_success('git', '-C', workspace, 'add', File.join('work', slug))
    assert_git_success('git', '-C', workspace, 'commit', '-m', 'terminal initiative')
  end

  def commit_archive_move(workspace, slug)
    assert_git_success(
      'git',
      '-C',
      workspace,
      'add',
      '-A',
      '--',
      File.join('work', slug),
      File.join('archive', slug)
    )
    assert_git_success('git', '-C', workspace, 'commit', '-m', 'archive initiative')
  end

  def set_lifecycle(workspace, slug, lifecycle)
    state = File.join(workspace, 'work', slug, 'state.md')
    content = File.read(state).sub(
      /\A---\nlifecycle: (?:active|complete|abandoned)\n---/,
      "---\nlifecycle: #{lifecycle}\n---"
    )
    File.write(state, content)
  end

  def state_with_body_lifecycle(content, lifecycle)
    fragment = "# Appendix\n\n- Lifecycle: #{lifecycle}\n\n"
    content.sub("## Results\n", "#{fragment}## Results\n")
  end

  def assert_git_success(*argv)
    stdout, stderr, status = Open3.capture3(*argv)
    assert(status.success?, "command failed: #{argv.join(' ')}\n#{stdout}\n#{stderr}")
  end

  def refute_git_success(*argv)
    stdout, stderr, status = Open3.capture3(*argv)
    message = "command unexpectedly succeeded: #{argv.join(' ')}\n#{stdout}\n#{stderr}"
    refute(status.success?, message)
  end

  def git_capture_success(*argv)
    stdout, stderr, status = Open3.capture3(*argv)
    assert(status.success?, "command failed: #{argv.join(' ')}\n#{stdout}\n#{stderr}")
    stdout
  end

  def configure_git_identity(path)
    assert_git_success('git', '-C', path, 'config', 'user.email', 'test@example.invalid')
    assert_git_success('git', '-C', path, 'config', 'user.name', 'Test User')
  end

  def tmux_capture(socket, *args)
    stdout, stderr, status = Open3.capture3('tmux', '-L', socket, *args)
    assert(status.success?, "tmux failed: #{args.join(' ')}\n#{stdout}\n#{stderr}")
    stdout
  end

  def tmux_run(socket, *args, allow_failure: false)
    _stdout, _stderr, status = Open3.capture3('tmux', '-L', socket, *args)
    assert(status.success?, "tmux failed: #{args.join(' ')}") unless allow_failure
    status
  end

  def tmux_session_exists?(socket, slug)
    target = DevSession::Tmux.session_target(slug)
    _stdout, _stderr, status = Open3.capture3('tmux', '-L', socket, 'has-session', '-t', target)
    status.success?
  end

  def wait_for_file(path)
    deadline = Time.now + 5

    until File.exist?(path)
      raise "timed out waiting for #{path}" if Time.now > deadline

      sleep 0.05
    end
  end

  def command_available?(cmd)
    ENV.fetch('PATH', '').split(File::PATH_SEPARATOR).any? do |dir|
      File.executable?(File.join(dir, cmd))
    end
  end

  def tmux_test_available?
    return false if ENV['DEV_SESSION_SKIP_REAL_TMUX_TESTS'] == '1'
    return @tmux_test_available unless @tmux_test_available.nil?
    return @tmux_test_available = false unless command_available?('tmux')

    socket = "dev-session-probe-#{Process.pid}-#{object_id}"
    shell = ENV.fetch('SHELL', '/bin/sh')
    _stdout, _stderr, status = Open3.capture3(
      'tmux', '-L', socket, 'new-session', '-d', '-s', 'probe', shell
    )
    _stdout, _stderr, live_status = Open3.capture3(
      'tmux', '-L', socket, 'has-session', '-t', '=probe:'
    )
    @tmux_test_available = status.success? && live_status.success?
    Open3.capture3('tmux', '-L', socket, 'kill-server') if @tmux_test_available
    @tmux_test_available
  end
end
