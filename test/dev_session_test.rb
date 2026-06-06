# frozen_string_literal: true

require 'date'
require 'fileutils'
require 'minitest/autorun'
require 'open3'
require 'shellwords'
require 'stringio'
require 'tmpdir'

load File.expand_path('../bin/dev-session', __dir__)

class DevSessionTest < Minitest::Test
  class NullTmux
    def managed_sessions
      []
    end

    def session_exists?(_slug)
      false
    end

    def managed_session?(_slug)
      false
    end

    def current_session
      nil
    end
  end

  class CurrentTmux < NullTmux
    def initialize(slug)
      @slug = slug
    end

    def current_session
      @slug
    end
  end

  TODAY = Date.new(2026, 6, 6)

  def test_build_slug_prefixes_current_date
    with_workspace do |workspace|
      runner = runner_for(workspace)

      assert_equal(
        '2026-06-06-api-token-rotation',
        runner.build_slug('api-token-rotation', as_is: false)
      )
      assert_equal(
        '2026-05-31-api-token-rotation',
        runner.build_slug('2026-05-31-api-token-rotation', as_is: true)
      )
    end
  end

  def test_slug_validation_rejects_paths_and_tmux_targets
    with_workspace do |workspace|
      runner = runner_for(workspace)

      assert_raises(VpsfreeDevSession::Error) do
        runner.build_slug('../escape', as_is: false)
      end

      assert_raises(VpsfreeDevSession::Error) do
        runner.build_slug('demo:1', as_is: false)
      end
    end
  end

  def test_lookup_slug_reports_ambiguity
    with_workspace do |workspace|
      FileUtils.mkdir_p(File.join(workspace, 'work', '2026-06-05-demo'))
      FileUtils.mkdir_p(File.join(workspace, 'worktrees', '2026-06-06-demo'))

      runner = runner_for(workspace)
      error = assert_raises(VpsfreeDevSession::Error) do
        runner.lookup_slug('demo', as_is: false)
      end

      assert_match(/ambiguous/, error.message)
      assert_match(/2026-06-05-demo/, error.message)
      assert_match(/2026-06-06-demo/, error.message)
    end
  end

  def test_start_slug_resolution_reuses_existing_unique_match
    with_workspace do |workspace|
      FileUtils.mkdir_p(File.join(workspace, 'work', '2026-06-05-service-health-checks'))

      runner = runner_for(workspace)

      assert_equal(
        '2026-06-05-service-health-checks',
        runner.resolve_start_slug('service-health-checks', as_is: false, new: false)
      )
    end
  end

  def test_start_slug_resolution_creates_today_slug_when_no_match_exists
    with_workspace do |workspace|
      runner = runner_for(workspace)

      assert_equal(
        '2026-06-06-demo',
        runner.resolve_start_slug('demo', as_is: false, new: false)
      )
    end
  end

  def test_start_slug_resolution_reports_ambiguity
    with_workspace do |workspace|
      FileUtils.mkdir_p(File.join(workspace, 'work', '2026-06-05-demo'))
      FileUtils.mkdir_p(File.join(workspace, 'work', '2026-06-06-demo'))

      runner = runner_for(workspace)
      error = assert_raises(VpsfreeDevSession::Error) do
        runner.resolve_start_slug('demo', as_is: false, new: false)
      end

      assert_match(/ambiguous/, error.message)
    end
  end

  def test_start_new_uses_today_slug_despite_existing_matches
    with_workspace do |workspace|
      FileUtils.mkdir_p(File.join(workspace, 'work', '2026-06-04-demo'))
      FileUtils.mkdir_p(File.join(workspace, 'work', '2026-06-05-demo'))

      runner = runner_for(workspace)

      assert_equal(
        '2026-06-06-demo',
        runner.resolve_start_slug('demo', as_is: false, new: true)
      )
    end
  end

  def test_start_new_reuses_today_slug_when_it_already_exists
    with_workspace do |workspace|
      FileUtils.mkdir_p(File.join(workspace, 'work', '2026-06-06-demo'))

      runner = runner_for(workspace)

      assert_equal(
        '2026-06-06-demo',
        runner.resolve_start_slug('demo', as_is: false, new: true)
      )
    end
  end

  def test_start_new_and_as_is_are_mutually_exclusive
    with_workspace do |workspace|
      runner = runner_for(workspace)

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.resolve_start_slug('demo', as_is: true, new: true)
      end

      assert_match(/cannot be used together/, error.message)
    end
  end

  def test_start_new_rejects_dated_slug
    with_workspace do |workspace|
      runner = runner_for(workspace)

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.resolve_start_slug('2026-06-05-demo', as_is: false, new: true)
      end

      assert_match(/requires a short name/, error.message)
    end
  end

  def test_tracking_files_are_created_once
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-06-06-demo'

      runner.ensure_tracking_files(slug)
      plan = File.join(workspace, 'work', slug, 'plan.md')
      state = File.join(workspace, 'work', slug, 'state.md')

      File.write(plan, "custom plan\n")
      runner.ensure_tracking_files(slug)

      assert_equal("custom plan\n", File.read(plan))
      assert_includes(File.read(state), '## Commands run')
      assert(File.directory?(File.join(workspace, 'worktrees', slug)))
    end
  end

  def test_list_output_uses_aligned_columns_for_long_slugs
    with_workspace do |workspace|
      short_slug = '2026-06-06-short'
      long_slug = '2026-06-06-this-is-a-longer-development-session-name'

      FileUtils.mkdir_p(File.join(workspace, 'work', short_slug))
      FileUtils.mkdir_p(File.join(workspace, 'work', long_slug))

      worktree = File.join(workspace, 'worktrees', long_slug, 'vpsadmin')
      FileUtils.mkdir_p(worktree)
      File.write(File.join(worktree, '.git'), "gitdir: /tmp/nonexistent\n")

      out = StringIO.new
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        out:,
        err: StringIO.new,
        today: TODAY
      )

      runner.list

      lines = out.string.lines.map(&:chomp)
      header = lines.fetch(0)
      work_column = header.index('WORK')
      worktrees_column = header.index('WORKTREES')
      tmux_column = header.index('TMUX')

      assert_equal(3, lines.length)

      lines.drop(1).each do |line|
        assert_equal('yes', line[work_column, 3])
        assert_match(/[0-9]/, line[worktrees_column, 'WORKTREES'.length])
        assert_equal('none', line[tmux_column, 4])
      end
    end
  end

  def test_current_uses_environment_slug
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      out = StringIO.new
      runner = runner_for(
        workspace,
        env: { VpsfreeDevSession::ENV_SLUG => slug },
        out:
      )

      assert_equal(slug, runner.current)
      assert_equal("#{slug}\n", out.string)
    end
  end

  def test_current_uses_managed_tmux_session_slug
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      out = StringIO.new
      runner = runner_for(workspace, tmux: CurrentTmux.new(slug), out:)

      assert_equal(slug, runner.current)
      assert_equal("#{slug}\n", out.string)
    end
  end

  def test_current_uses_work_directory_slug
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      cwd = File.join(workspace, 'work', slug, 'notes')
      FileUtils.mkdir_p(cwd)

      runner = runner_for(workspace, cwd:)

      assert_equal(slug, runner.current_slug)
    end
  end

  def test_current_uses_worktrees_directory_slug
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      cwd = File.join(workspace, 'worktrees', slug, 'vpsadmin', 'app')
      FileUtils.mkdir_p(cwd)

      runner = runner_for(workspace, cwd:)

      assert_equal(slug, runner.current_slug)
    end
  end

  def test_current_reports_missing_active_session
    with_workspace do |workspace|
      runner = runner_for(workspace)

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.current
      end

      assert_match(/no current dev session/, error.message)
    end
  end

  def test_current_reports_conflicting_sources
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      other_slug = '2026-06-06-other'
      cwd = File.join(workspace, 'work', other_slug)
      FileUtils.mkdir_p(cwd)

      runner = runner_for(
        workspace,
        env: { VpsfreeDevSession::ENV_SLUG => slug },
        cwd:
      )

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.current
      end

      assert_match(/sources disagree/, error.message)
      assert_match(/#{VpsfreeDevSession::ENV_SLUG}=#{slug}/, error.message)
      assert_match(/cwd=#{other_slug}/, error.message)
    end
  end

  def test_worktree_add_and_remove_keep_branch
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')

      runner = runner_for(workspace)
      runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      path = File.join(workspace, 'worktrees', '2026-06-06-demo', 'sample')
      assert(File.exist?(File.join(path, '.git')))

      runner.worktree_remove('demo', 'sample', as_is: false, force: false)

      refute(File.exist?(path))
      assert_git_success(
        'git',
        "--git-dir=#{File.join(workspace, 'repos', 'sample.git')}",
        'show-ref',
        '--verify',
        '--quiet',
        'refs/heads/2026-06-06-demo'
      )
    end
  end

  def test_remove_cleans_worktrees_but_keeps_notes_and_branch
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')

      runner = runner_for(workspace)
      runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      slug = '2026-06-06-demo'
      runner.remove('demo', as_is: false, force: false, all: false)

      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
      assert(File.exist?(File.join(workspace, 'work', slug, 'plan.md')))
      assert_git_success(
        'git',
        "--git-dir=#{File.join(workspace, 'repos', 'sample.git')}",
        'show-ref',
        '--verify',
        '--quiet',
        'refs/heads/2026-06-06-demo'
      )
    end
  end

  def test_remove_all_deletes_notes
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-06-06-demo'

      runner.ensure_tracking_files(slug)
      runner.remove('demo', as_is: false, force: false, all: true)

      refute(File.exist?(File.join(workspace, 'work', slug)))
      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
    end
  end

  def test_remove_refuses_dirty_worktrees_without_force
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')

      runner = runner_for(workspace)
      runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      path = File.join(workspace, 'worktrees', '2026-06-06-demo', 'sample')
      File.write(File.join(path, 'dirty.txt'), "dirty\n")

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.remove('demo', as_is: false, force: false, all: false)
      end

      assert_match(/uncommitted changes/, error.message)
      assert(File.exist?(path))
    end
  end

  def test_remove_force_cleans_dirty_worktrees
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')

      runner = runner_for(workspace)
      runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      slug = '2026-06-06-demo'
      path = File.join(workspace, 'worktrees', slug, 'sample')
      File.write(File.join(path, 'dirty.txt'), "dirty\n")

      runner.remove('demo', as_is: false, force: true, all: false)

      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
      assert(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_remove_kills_managed_tmux_session
    skip 'tmux is not available' unless command_available?('tmux')

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'

    with_workspace do |workspace|
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        codex_command: 'false',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY
      )

      runner.start('demo', as_is: false, new: false, attach: false, run_codex: false)
      assert(tmux_session_exists?(socket, slug))

      runner.remove('demo', as_is: false, force: false, all: false)

      refute(tmux_session_exists?(socket, slug))
      assert(File.exist?(File.join(workspace, 'work', slug)))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_remove_refuses_unmanaged_tmux_session
    skip 'tmux is not available' unless command_available?('tmux')

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'

    with_workspace do |workspace|
      FileUtils.mkdir_p(File.join(workspace, 'work', slug))
      tmux_run(socket, 'new-session', '-d', '-s', slug, '-c', workspace)

      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        codex_command: 'false',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY
      )

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.remove('demo', as_is: false, force: false, all: false)
      end

      assert_match(/not managed/, error.message)
      assert(tmux_session_exists?(socket, slug))
      assert(File.exist?(File.join(workspace, 'work', slug)))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_tmux_start_and_sync_manage_only_worktree_windows
    skip 'tmux is not available' unless command_available?('tmux')

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'

    with_workspace do |workspace|
      worktree = File.join(workspace, 'worktrees', slug, 'alpha')
      FileUtils.mkdir_p(worktree)
      File.write(File.join(worktree, '.git'), "gitdir: /tmp/nonexistent\n")

      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        codex_command: 'false',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY
      )

      runner.start('demo', as_is: false, new: false, attach: false, run_codex: false)

      session_env = tmux_capture(socket, 'show-environment', '-t', slug)
      assert_includes(session_env, "#{VpsfreeDevSession::ENV_SLUG}=#{slug}\n")
      assert_includes(
        session_env,
        "#{VpsfreeDevSession::ENV_WORKSPACE}=#{workspace}\n"
      )
      assert_includes(
        session_env,
        "#{VpsfreeDevSession::ENV_WORK_DIR}=#{File.join(workspace, 'work', slug)}\n"
      )
      assert_includes(
        session_env,
        "#{VpsfreeDevSession::ENV_WORKTREES_DIR}=#{File.join(workspace, 'worktrees', slug)}\n"
      )

      panes = tmux_capture(socket, 'list-panes', '-t', "#{slug}:dev", '-F', '#{pane_current_path}')
              .lines
              .map(&:chomp)

      assert_equal(3, panes.length)
      assert_includes(panes, workspace)
      assert_includes(panes, File.join(workspace, 'work', slug))
      assert_includes(panes, File.join(workspace, 'worktrees', slug))

      windows = tmux_capture(
        socket,
        'list-windows',
        '-t',
        slug,
        '-F',
        '#{window_name}:#{@vpsfree_dev_session_window}'
      ).lines.map(&:chomp)

      assert_includes(windows, 'alpha:worktree')

      probe = File.join(workspace, 'alpha-env.txt')
      command = "printf '%s\\n' \"$#{VpsfreeDevSession::ENV_SLUG}\" > #{Shellwords.escape(probe)}"
      tmux_run(socket, 'send-keys', '-t', "#{slug}:alpha", command, 'Enter')
      wait_for_file(probe)
      assert_equal(slug, File.read(probe).strip)

      tmux_run(socket, 'new-window', '-d', '-t', slug, '-n', 'custom', '-c', workspace)
      FileUtils.rm_rf(worktree)
      runner.sync('demo', as_is: false)

      names = tmux_capture(socket, 'list-windows', '-t', slug, '-F', '#{window_name}')
              .lines
              .map(&:chomp)

      assert_includes(names, 'custom')
      refute_includes(names, 'alpha')
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  private

  def with_workspace
    Dir.mktmpdir('dev-session-test') do |workspace|
      FileUtils.mkdir_p(File.join(workspace, 'repos'))
      FileUtils.mkdir_p(File.join(workspace, 'work'))
      FileUtils.mkdir_p(File.join(workspace, 'worktrees'))
      yield workspace
    end
  end

  def runner_for(workspace, env: {}, cwd: nil, tmux: nil, out: nil)
    out ||= StringIO.new
    tmux ||= NullTmux.new

    VpsfreeDevSession::Runner.new(
      workspace:,
      tmux:,
      out:,
      err: StringIO.new,
      today: TODAY,
      env:,
      cwd: cwd || workspace
    )
  end

  def create_bare_repo(workspace, project)
    source = File.join(workspace, 'source')
    bare = File.join(workspace, 'repos', "#{project}.git")

    assert_git_success('git', 'init', '-b', 'master', source)
    assert_git_success('git', '-C', source, 'config', 'user.email', 'test@example.invalid')
    assert_git_success('git', '-C', source, 'config', 'user.name', 'Test User')
    File.write(File.join(source, 'README.md'), "# Test\n")
    assert_git_success('git', '-C', source, 'add', 'README.md')
    assert_git_success('git', '-C', source, 'commit', '-m', 'initial')
    assert_git_success('git', 'clone', '--bare', source, bare)
  end

  def assert_git_success(*argv)
    stdout, stderr, status = Open3.capture3(*argv)
    assert(status.success?, "command failed: #{argv.join(' ')}\n#{stdout}\n#{stderr}")
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
    _stdout, _stderr, status = Open3.capture3('tmux', '-L', socket, 'has-session', '-t', slug)
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
end
