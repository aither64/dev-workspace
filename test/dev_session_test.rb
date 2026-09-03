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
    def session(_slug)
      nil
    end

    def session_by_id(_id)
      nil
    end

    def managed_session_identities
      []
    end

    def current_session_identity(_pane)
      nil
    end

  end

  class CurrentTmux < NullTmux
    def initialize(slug, workspace:)
      @slug = slug
      @workspace = workspace
    end

    def current_session_identity(_pane)
      VpsfreeDevSession::Tmux::Session.new(
        id: '$current',
        name: @slug,
        mark: '1',
        slug: @slug,
        workspace: @workspace
      )
    end
  end

  class ManagedTmux < NullTmux
    attr_reader :killed

    def initialize(slug, workspace:, on_kill: nil)
      @slug = slug
      @workspace = workspace
      @id = '$managed'
      @on_kill = on_kill
      @killed = false
    end

    def session(slug)
      return unless slug == @slug && !@killed

      identity
    end

    def session_by_id(id)
      return unless id == @id && !@killed

      identity
    end

    def managed_session_identities
      @killed ? [] : [identity]
    end

    def windows(_session)
      []
    end

    def argv(*args)
      ['tmux', *args]
    end

    def run(*args)
      targets = ["#{@id}:", @slug]
      return unless args.first(2) == ['kill-session', '-t'] && targets.include?(args[2])

      @on_kill&.call
      @killed = true
    end

    private

    def identity
      VpsfreeDevSession::Tmux::Session.new(
        id: @id,
        name: @slug,
        mark: '1',
        slug: @slug,
        workspace: @workspace
      )
    end
  end

  class LegacyWorkspaceTmux < ManagedTmux
    attr_reader :workspace

    def initialize(slug, workspace:)
      super
      @workspace = workspace
    end

    def run(*args)
      if args.first == 'set-environment' &&
         args[-2] == VpsfreeDevSession::ENV_WORKSPACE
        @workspace = args.last
      else
        super
      end
    end
  end

  class UnmanagedTmux < NullTmux
    def initialize(slug)
      @slug = slug
    end

    def session(slug)
      return unless slug == @slug

      VpsfreeDevSession::Tmux::Session.new(
        id: '$unmanaged',
        name: @slug,
        mark: '',
        slug: '',
        workspace: ''
      )
    end

  end

  class ReplacedTmux < NullTmux
    attr_reader :kill_attempted

    def initialize(slug, workspace:)
      @slug = slug
      @workspace = workspace
      @kill_attempted = false
    end

    def session(slug)
      return unless slug == @slug

      VpsfreeDevSession::Tmux::Session.new(
        id: '$original',
        name: @slug,
        mark: '1',
        slug: @slug,
        workspace: @workspace
      )
    end

    def session_by_id(_id)
      nil
    end

    def run(*_args)
      @kill_attempted = true
    end
  end

  class ReplacedDuringCreateTmux < NullTmux
    attr_reader :mutations, :new_session_args, :name_lookups

    def initialize(slug, workspace:)
      @slug = slug
      @workspace = workspace
      @created = false
      @mutations = []
      @name_lookups = 0
    end

    def session(slug)
      @name_lookups += 1
      return unless @created && slug == @slug

      VpsfreeDevSession::Tmux::Session.new(
        id: '$replacement',
        name: @slug,
        mark: '',
        slug: '',
        workspace: @workspace
      )
    end

    def session_by_id(_id)
      nil
    end

    def capture(*args, allow_failure: false)
      raise "unexpected allow_failure: #{args.inspect}" if allow_failure
      raise "unexpected tmux capture: #{args.inspect}" unless args.first == 'new-session'

      @created = true
      @new_session_args = args
      ["$original\n", '', nil]
    end

    def run(*args)
      @mutations << args
    end
  end

  class ReplacedBeforeSyncTmux < NullTmux
    attr_reader :mutations, :name_lookups

    def initialize(slug, workspace:)
      @slug = slug
      @workspace = workspace
      @created = false
      @id_lookups = 0
      @mutations = []
      @name_lookups = 0
      @pane = 0
    end

    def session(slug)
      @name_lookups += 1
      return unless @created && slug == @slug

      identity('$replacement', managed: true)
    end

    def session_by_id(id)
      return identity(id, managed: true) if id == '$replacement'
      return unless id == '$original'

      @id_lookups += 1
      case @id_lookups
      when 1 then identity(id, managed: false)
      when 2 then identity(id, managed: true)
      end
    end

    def capture(*args, allow_failure: false)
      raise "unexpected allow_failure: #{args.inspect}" if allow_failure

      output = case args.first
               when 'new-session'
                 @created = true
                 '$original'
               when 'display-message'
                 '%left'
               when 'split-window'
                 @pane += 1
                 "%pane#{@pane}"
               else
                 raise "unexpected tmux capture: #{args.inspect}"
               end
      ["#{output}\n", '', nil]
    end

    def run(*args)
      @mutations << args
    end

    def windows(_session)
      []
    end

    private

    def identity(id, managed:)
      VpsfreeDevSession::Tmux::Session.new(
        id:,
        name: @slug,
        mark: managed ? '1' : '',
        slug: managed ? @slug : '',
        workspace: @workspace
      )
    end
  end

  class PartialCreateTmux < NullTmux
    attr_reader :kill_count, :split_attempts

    def initialize(slug, workspace:)
      @slug = slug
      @workspace = workspace
      @created = false
      @kill_count = 0
      @split_attempts = 0
      @mark = ''
      @session_slug = ''
    end

    def session(slug)
      return unless @created && slug == @slug

      identity
    end

    def session_by_id(id)
      return unless @created && id == '$partial'

      identity
    end

    def capture(*args, allow_failure: false)
      raise "unexpected allow_failure: #{args.inspect}" if allow_failure

      case args.first
      when 'new-session'
        @created = true
        @mark = ''
        @session_slug = ''
        ["$partial\n", '', nil]
      when 'display-message'
        ["%left\n", '', nil]
      when 'split-window'
        @split_attempts += 1
        raise VpsfreeDevSession::Error, 'split failed'
      else
        raise "unexpected tmux capture: #{args.inspect}"
      end
    end

    def run(*args)
      if args.first == 'set-option' && args[-2] == VpsfreeDevSession::SESSION_MARK
        @mark = args.last
      elsif args.first == 'set-option' && args[-2] == VpsfreeDevSession::SESSION_SLUG
        @session_slug = args.last
      elsif args.first == 'kill-session'
        @created = false
        @kill_count += 1
      end
    end

    private

    def identity
      VpsfreeDevSession::Tmux::Session.new(
        id: '$partial',
        name: @slug,
        mark: @mark,
        slug: @session_slug,
        workspace: @workspace
      )
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

  def test_lookup_slug_ignores_an_unsafe_legacy_managed_session_name
    with_workspace do |workspace|
      tmux = ManagedTmux.new('x/2026-06-06-demo', workspace:)
      runner = runner_for(workspace, tmux:)

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.lookup_slug('demo', as_is: false)
      end

      assert_match(/no slug found/, error.message)
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

  def test_start_as_is_rejects_an_unsafe_slug
    with_workspace do |workspace|
      runner = runner_for(workspace)

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.resolve_start_slug('../escape', as_is: true, new: false)
      end

      assert_match(/invalid slug/, error.message)
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
      assert_includes(File.read(state), '- Lifecycle: active')
      assert(File.directory?(File.join(workspace, 'worktrees', slug)))
    end
  end

  def test_tracking_files_refuse_an_archived_slug
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      FileUtils.mkdir_p(File.join(workspace, 'archive', slug))

      error = assert_raises(VpsfreeDevSession::Error) do
        runner_for(workspace).ensure_tracking_files(slug)
      end

      assert_match(/archived slug cannot be reused/, error.message)
      refute(File.exist?(File.join(workspace, 'work', slug)))
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
        env: {
          VpsfreeDevSession::ENV_SLUG => slug,
          VpsfreeDevSession::ENV_WORKSPACE => workspace
        },
        out:
      )

      assert_equal(slug, runner.current)
      assert_equal("#{slug}\n", out.string)
    end
  end

  def test_current_rejects_an_environment_from_another_workspace
    with_workspace do |workspace|
      Dir.mktmpdir('foreign-dev-session-workspace') do |foreign_workspace|
        runner = runner_for(
          workspace,
          env: {
            VpsfreeDevSession::ENV_SLUG => '2026-06-06-demo',
            VpsfreeDevSession::ENV_WORKSPACE => foreign_workspace
          }
        )

        error = assert_raises(VpsfreeDevSession::Error) { runner.current }

        assert_match(/not managed by this workspace/, error.message)
      end
    end
  end

  def test_current_uses_managed_tmux_session_slug
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      out = StringIO.new
      runner = runner_for(
        workspace,
        tmux: CurrentTmux.new(slug, workspace:),
        env: { 'TMUX' => 'socket', 'TMUX_PANE' => '%1' },
        out:
      )

      assert_equal(slug, runner.current)
      assert_equal("#{slug}\n", out.string)
    end
  end

  def test_current_rejects_a_tmux_session_from_another_workspace
    with_workspace do |workspace|
      Dir.mktmpdir('foreign-dev-session-workspace') do |foreign_workspace|
        tmux = CurrentTmux.new('2026-06-06-demo', workspace: foreign_workspace)
        runner = runner_for(
          workspace,
          tmux:,
          env: { 'TMUX' => 'socket', 'TMUX_PANE' => '%1' }
        )

        error = assert_raises(VpsfreeDevSession::Error) { runner.current }

        assert_match(/not managed by this workspace/, error.message)
      end
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

  def test_current_ignores_tmux_server_current_session_outside_a_tmux_pane
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
        today: TODAY,
        env: {},
        cwd: workspace
      )
      runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)

      error = assert_raises(VpsfreeDevSession::Error) { runner.current }

      assert_match(/no current dev session/, error.message)
      assert(tmux_session_exists?(socket, slug))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_devcluster_shorthands_use_the_workspace_session_resolver
    scripts = %w[
      dev-clusters/vpsadmin/bin/devcluster
      dev-clusters/vpsadminos/bin/devcluster
    ]

    Dir.mktmpdir('dev-session-resolver') do |directory|
      resolver = File.join(directory, 'dev-session')
      File.write(
        resolver,
        "#!/usr/bin/env bash\n" \
        "test \"$1\" = current\n" \
        "printf 'managed-session\\n'\n"
      )
      FileUtils.chmod(0o755, resolver)

      scripts.each do |relative_path|
        source = File.read(File.expand_path("../#{relative_path}", __dir__))
        function = source[/^current_slug\(\) \{\n.*?^\}\n/m]
        refute_nil(function, relative_path)

        stdout, stderr, status = Open3.capture3(
          { 'VPSFREE_DEV_SESSION_SLUG' => 'foreign-session' },
          'bash',
          '-c',
          "#{function}\nDEV_SESSION_BIN=\"$1\"\ncurrent_slug\n",
          'devcluster-current-slug-test',
          resolver
        )

        assert(status.success?, "#{relative_path}: #{stderr}")
        assert_equal("managed-session\n", stdout, relative_path)
      end
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
        env: {
          VpsfreeDevSession::ENV_SLUG => slug,
          VpsfreeDevSession::ENV_WORKSPACE => workspace
        },
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
      runner.remove('demo', as_is: false, force: false)

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

  def test_remove_kills_session_after_worktrees_are_removed
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')

      add_runner = runner_for(workspace)
      add_runner.worktree_add(
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
      tmux = ManagedTmux.new(
        slug,
        workspace:,
        on_kill: -> { refute(File.exist?(path)) }
      )
      remove_runner = runner_for(workspace, tmux:)

      remove_runner.remove('demo', as_is: false, force: false)

      assert(tmux.killed)
      refute(File.exist?(File.join(workspace, 'worktrees', slug)))
      assert(File.exist?(File.join(workspace, 'work', slug)))
    end
  end

  def test_remove_all_is_rejected
    with_workspace do |workspace|
      err = StringIO.new
      status = VpsfreeDevSession::CLI.new(
        ['--workspace', workspace, 'remove', 'demo', '--all'],
        out: StringIO.new,
        err:
      ).run

      assert_equal(1, status)
      assert_match(/invalid option: --all/, err.string)
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
        runner.remove('demo', as_is: false, force: false)
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

      runner.remove('demo', as_is: false, force: true)

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

      runner.remove('demo', as_is: false, force: false)

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
        runner.remove('demo', as_is: false, force: false)
      end

      assert_match(/not managed/, error.message)
      assert(tmux_session_exists?(socket, slug))
      assert(File.exist?(File.join(workspace, 'work', slug)))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_finalize_archives_tracking_after_removing_worktrees
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      create_bare_repo(workspace, 'sample')

      add_runner = runner_for(workspace)
      add_runner.worktree_add(
        'demo',
        'sample',
        as_is: false,
        name: nil,
        branch: nil,
        base: 'master',
        fetch: false
      )

      slug = '2026-06-06-demo'
      commit_tracking(workspace, slug, lifecycle: 'complete')
      out = StringIO.new
      tmux = ManagedTmux.new(
        slug,
        workspace:,
        on_kill: lambda do
          refute(File.exist?(File.join(workspace, 'work', slug)))
          refute(File.exist?(File.join(workspace, 'worktrees', slug)))
          assert(File.directory?(File.join(workspace, 'archive', slug)))
        end
      )

      runner_for(workspace, tmux:, out:).finalize('demo', as_is: false)

      assert(tmux.killed)
      assert_includes(out.string, File.join(workspace, 'archive', slug))
      assert_includes(
        File.read(File.join(workspace, 'archive', slug, 'state.md')),
        '- Lifecycle: complete'
      )
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

  def test_finalize_accepts_abandoned_lifecycle
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'abandoned')

      runner.finalize('demo', as_is: false)

      assert(File.directory?(File.join(workspace, 'archive', slug)))
    end
  end

  def test_finalize_refuses_active_lifecycle
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'active')

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.finalize('demo', as_is: false)
      end

      assert_match(/lifecycle is not terminal/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_missing_tracking_file
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      FileUtils.rm(File.join(workspace, 'work', slug, 'plan.md'))

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.finalize('demo', as_is: false)
      end

      assert_match(/missing tracking files/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_tracking_without_a_prior_commit
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      set_lifecycle(workspace, slug, 'complete')

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.finalize('demo', as_is: false)
      end

      assert_match(/tracking files have no prior commit/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_existing_archive
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      FileUtils.mkdir_p(File.join(workspace, 'archive', slug))

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.finalize('demo', as_is: false)
      end

      assert_match(/archive already exists/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_dirty_worktree_without_changing_session
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
      commit_tracking(workspace, slug, lifecycle: 'complete')
      path = File.join(workspace, 'worktrees', slug, 'sample')
      File.write(File.join(path, 'dirty.txt'), "dirty\n")
      tmux = ManagedTmux.new(slug, workspace:)

      error = assert_raises(VpsfreeDevSession::Error) do
        runner_for(workspace, tmux:).finalize('demo', as_is: false)
      end

      assert_match(/uncommitted changes/, error.message)
      refute(tmux.killed)
      assert(File.directory?(path))
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_unmanaged_worktree_group_entries
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')
      FileUtils.mkdir_p(File.join(workspace, 'worktrees', slug, 'cache'))

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.finalize('demo', as_is: false)
      end

      assert_match(/contains unmanaged entries/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_finalize_refuses_unmanaged_tmux_session
    skip 'git is not available' unless command_available?('git')

    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      commit_tracking(workspace, slug, lifecycle: 'complete')

      error = assert_raises(VpsfreeDevSession::Error) do
        runner_for(workspace, tmux: UnmanagedTmux.new(slug)).finalize(
          'demo',
          as_is: false
        )
      end

      assert_match(/not managed/, error.message)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_tmux_targets_do_not_prefix_match_a_longer_session
    skip 'tmux is not available' unless command_available?('tmux')

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'
    longer_slug = "#{slug}-long"

    with_workspace do |workspace|
      tmux_run(socket, 'new-session', '-d', '-s', longer_slug, '-c', workspace)
      command_runner = VpsfreeDevSession::CommandRunner.new(
        out: StringIO.new,
        err: StringIO.new
      )
      tmux = VpsfreeDevSession::Tmux.new(runner: command_runner, socket:)

      refute(tmux.session(slug))
      assert(tmux.session(longer_slug))

      error = assert_raises(VpsfreeDevSession::Error) do
        runner_for(workspace, tmux:).stop(slug, as_is: true)
      end

      assert_match(/session not found/, error.message)
      assert(tmux_session_exists?(socket, longer_slug))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_start_does_not_adopt_a_replacement_tmux_session
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      tmux = ReplacedDuringCreateTmux.new(slug, workspace:)
      runner = runner_for(workspace, tmux:)

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)
      end

      assert_match(/changed during creation/, error.message)
      assert_includes(tmux.new_session_args, '-P')
      assert_includes(tmux.new_session_args, '#{session_id}')
      assert_equal(1, tmux.name_lookups)
      assert_empty(tmux.mutations)
    end
  end

  def test_start_keeps_the_created_identity_through_sync
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      tmux = ReplacedBeforeSyncTmux.new(slug, workspace:)
      runner = runner_for(workspace, tmux:)

      error = assert_raises(VpsfreeDevSession::Error) do
        runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)
      end

      assert_match(/session changed during operation/, error.message)
      assert_equal(1, tmux.name_lookups)
      refute(tmux.mutations.flatten.include?('$replacement:'))
    end
  end

  def test_start_prints_an_identity_bound_attach_command
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      out = StringIO.new
      tmux = ManagedTmux.new(slug, workspace:)
      runner = runner_for(workspace, tmux:, out:)

      runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)

      expected = ['tmux', 'attach-session', '-t', '$managed:']
                 .map(&:shellescape)
                 .join(' ')
      assert_includes(out.string, "attach: #{expected}")
      refute_includes(out.string, "=#{slug}:")
    end
  end

  def test_start_normalizes_a_legacy_symlinked_workspace_identity
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      Dir.mktmpdir('dev-session-workspace-alias') do |directory|
        alias_path = File.join(directory, 'workspace')
        FileUtils.ln_s(workspace, alias_path)
        out = StringIO.new
        tmux = LegacyWorkspaceTmux.new(slug, workspace: alias_path)
        runner = runner_for(workspace, tmux:, out:)

        runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)

        assert_equal(workspace, tmux.workspace)
        assert_includes(out.string, '$managed:')
      end
    end
  end

  def test_start_rolls_back_a_partial_tmux_layout_and_retries_creation
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      tmux = PartialCreateTmux.new(slug, workspace:)
      runner = runner_for(workspace, tmux:)

      2.times do
        error = assert_raises(VpsfreeDevSession::Error) do
          runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)
        end
        assert_match(/split failed/, error.message)
      end

      assert_equal(2, tmux.split_attempts)
      assert_equal(2, tmux.kill_count)
    end
  end

  def test_worktree_mutations_use_the_slug_lock
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      lock_path = File.join(workspace, 'worktrees', '.locks', "#{slug}.lock")
      FileUtils.mkdir_p(File.dirname(lock_path))

      File.open(lock_path, File::RDWR | File::CREAT, 0o600) do |lock|
        assert(lock.flock(File::LOCK_EX | File::LOCK_NB))

        add_error = assert_raises(VpsfreeDevSession::Error) do
          runner.worktree_add(
            slug,
            'sample',
            as_is: true,
            name: nil,
            branch: nil,
            base: nil,
            fetch: false
          )
        end
        assert_match(/another dev-session command/, add_error.message)

        remove_error = assert_raises(VpsfreeDevSession::Error) do
          runner.worktree_remove(slug, 'sample', as_is: true, force: false)
        end
        assert_match(/another dev-session command/, remove_error.message)
      end
    end
  end

  def test_stop_does_not_kill_a_replacement_tmux_session
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner_for(workspace).ensure_tracking_files(slug)
      tmux = ReplacedTmux.new(slug, workspace:)

      error = assert_raises(VpsfreeDevSession::Error) do
        runner_for(workspace, tmux:).stop(slug, as_is: true)
      end

      assert_match(/session changed during operation/, error.message)
      refute(tmux.kill_attempted)
    end
  end

  def test_remove_does_not_kill_a_replacement_tmux_session
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      runner_for(workspace).ensure_tracking_files(slug)
      tmux = ReplacedTmux.new(slug, workspace:)

      error = assert_raises(VpsfreeDevSession::Error) do
        runner_for(workspace, tmux:).remove(slug, as_is: true, force: false)
      end

      assert_match(/session changed during operation/, error.message)
      refute(tmux.kill_attempted)
      assert(File.directory?(File.join(workspace, 'work', slug)))
    end
  end

  def test_managed_tmux_session_is_bound_to_its_workspace
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
      pane = tmux_capture(socket, 'list-panes', '-t', slug, '-F', '#{pane_id}')
             .lines
             .first
             .strip

      Dir.mktmpdir('other-dev-session-workspace') do |other_workspace|
        other_out = StringIO.new
        other_runner = VpsfreeDevSession::Runner.new(
          workspace: other_workspace,
          tmux_socket: socket,
          out: other_out,
          err: StringIO.new,
          today: TODAY,
          env: { 'TMUX' => 'socket', 'TMUX_PANE' => pane }
        )

        current_error = assert_raises(VpsfreeDevSession::Error) do
          other_runner.current
        end
        assert_match(/not managed by this workspace/, current_error.message)

        other_runner.list
        assert_equal('', other_out.string)
        lookup_error = assert_raises(VpsfreeDevSession::Error) do
          other_runner.lookup_slug('demo', as_is: false)
        end
        assert_match(/no slug found/, lookup_error.message)

        error = assert_raises(VpsfreeDevSession::Error) do
          other_runner.stop(slug, as_is: true)
        end

        assert_match(/not managed by this workspace/, error.message)
      end

      assert(tmux_session_exists?(socket, slug))
    ensure
      tmux_run(socket, 'kill-server', allow_failure: true)
    end
  end

  def test_tmux_codex_runs_from_shell_and_leaves_shell_available
    skip 'tmux is not available' unless command_available?('tmux')

    socket = "dev-session-test-#{Process.pid}-#{object_id}"
    slug = '2026-06-06-demo'

    with_workspace do |workspace|
      codex_probe = File.join(workspace, 'codex-ran.txt')
      shell_probe = File.join(workspace, 'shell-remained.txt')
      codex_command = "printf codex > #{Shellwords.escape(codex_probe)}"
      runner = VpsfreeDevSession::Runner.new(
        workspace:,
        tmux_socket: socket,
        codex_command:,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY
      )

      runner.start('demo', as_is: false, new: false, attach: false, run_codex: true)
      wait_for_file(codex_probe)

      panes = tmux_capture(
        socket,
        'list-panes',
        '-t',
        "#{slug}:dev",
        '-F',
        "\#{pane_id}\t\#{pane_current_path}"
      ).lines.map { |line| line.chomp.split("\t", 2) }
      left = panes.find { |_id, path| path == workspace }.fetch(0)
      command = "printf shell > #{Shellwords.escape(shell_probe)}"

      tmux_run(socket, 'send-keys', '-t', left, '-l', command)
      tmux_run(socket, 'send-keys', '-t', left, 'Enter')
      wait_for_file(shell_probe)

      assert_equal('codex', File.read(codex_probe))
      assert_equal('shell', File.read(shell_probe))
      assert(tmux_session_exists?(socket, slug))
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

  def commit_tracking(workspace, slug, lifecycle:)
    set_lifecycle(workspace, slug, lifecycle)
    assert_git_success('git', 'init', '-b', 'master', workspace)
    assert_git_success('git', '-C', workspace, 'config', 'user.email', 'test@example.invalid')
    assert_git_success('git', '-C', workspace, 'config', 'user.name', 'Test User')
    assert_git_success('git', '-C', workspace, 'add', File.join('work', slug))
    assert_git_success('git', '-C', workspace, 'commit', '-m', 'track initiative')
  end

  def set_lifecycle(workspace, slug, lifecycle)
    state = File.join(workspace, 'work', slug, 'state.md')
    content = File.read(state).sub('- Lifecycle: active', "- Lifecycle: #{lifecycle}")
    File.write(state, content)
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
    target = VpsfreeDevSession::Tmux.session_target(slug)
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
end
