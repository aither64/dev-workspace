# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_archive_retirement_proves_the_retained_set_before_journal_or_cleanup
    %w[unknown-thread busy queued unresolved creating replacement removal].each do |failure|
      with_archive_retirement_fixture do |runner, workspace, slug, control, log|
        File.write(control, failure)
        error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
        assert_includes(error.message, failure)
        refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
        refute(File.exist?(File.join(workspace, 'worktrees', '.locks', "#{slug}.archive-cleanup.json")))
        assert(File.directory?(File.join(workspace, 'work', slug)))
        assert_equal([%w[team require-archive-ready]], File.readlines(log).map { |line| JSON.parse(line).first(2) })
      end
    end
  end

  def test_archive_retirement_archives_only_retained_members_before_active_or_archived_root
    %w[active archived].each do |root_state|
      with_archive_retirement_fixture(root_state:) do |runner, workspace, slug, _control, log|
        runner.archive(slug, as_is: true)
        commands = File.readlines(log).map { |line| JSON.parse(line) }
        retirement = commands.select { |argv| argv.first == 'team' && %w[archive require-archived].include?(argv[1]) || argv.first(2) == %w[thread retire] }
        assert_equal([%w[team archive], %w[team require-archived], %w[thread retire]], retirement.map { |argv| argv.first(2) })
        assert_includes(retirement.first, '--retained-only')
        refute(retirement.flatten.include?('--force'))
        commands.select { |argv| argv.first == 'team' }.each do |argv|
          assert_equal(File.join(workspace, 'work', slug), argv.fetch(argv.index('--cwd') + 1))
          assert_equal(File.join(workspace, '.codex'), argv.fetch(argv.index('--codex-home') + 1))
          assert_equal(File.join(workspace, '.authority'), argv.fetch(argv.index('--authority-dir') + 1))
        end
        refute(File.exist?(runner.send(:lifecycle_journal_file, slug, 'archive')))
      end
    end
  end

  def test_archive_retirement_retries_partial_team_proof_and_lost_root_acknowledgement
    %w[team team-proof root].each do |failure|
      with_archive_retirement_fixture do |runner, workspace, slug, control, log|
        File.write(control, failure)
        assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
        journal_path = runner.send(:lifecycle_journal_file, slug, 'archive')
        journal = JSON.parse(File.read(journal_path))
        assert_equal('tracking_committed', journal.fetch('phase'))
        committed = git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD').strip
        commands = File.readlines(log).map { |line| JSON.parse(line).first(2) }
        refute_includes(commands, %w[thread retire]) unless failure == 'root'
        File.unlink(control)
        runner.archive(slug, as_is: true)
        assert_equal(committed, git_capture_success('git', '-C', workspace, 'rev-parse', 'HEAD').strip)
        refute(File.exist?(journal_path))
        state = JSON.parse(File.read("#{control}.state"))
        assert_equal('archived', state.fetch('root'))
        assert_equal(%w[archived archived], state.fetch('members'))
        assert_equal(failure == 'root' ? 1 : 0, state.fetch('root_archives_before_retry'))
      end
    end
  end

  def test_archive_retirement_repeats_retained_set_proof_at_finalization_before_cleanup
    with_archive_retirement_fixture do |runner, workspace, slug, control, log|
      File.write(control, 'cleanup-preflight')
      error = assert_raises(DevSession::Error) { runner.archive(slug, as_is: true) }
      assert_includes(error.message, 'cleanup preflight found an unknown conversation')
      assert(File.directory?(File.join(workspace, 'work', slug)))
      refute(File.exist?(File.join(workspace, 'archive', slug)))
      journal_path = runner.send(:lifecycle_journal_file, slug, 'archive')
      assert_equal('clusters_released', JSON.parse(File.read(journal_path)).fetch('phase'))
      commands = File.readlines(log).map { |line| JSON.parse(line).first(2) }
      refute_includes(commands, %w[team archive])
      refute_includes(commands, %w[thread retire])
      File.unlink(control)
      runner.archive(slug, as_is: true)
      refute(File.exist?(journal_path))
    end
  end

  private

  def with_archive_retirement_fixture(root_state: 'active')
    with_workspace do |workspace|
      slug = '2026-06-06-retained-retirement'
      control = File.join(workspace, '.retirement-control')
      log = "#{control}.log"
      state_path = "#{control}.state"
      File.write(state_path, JSON.generate('root' => root_state, 'members' => %w[archived active], 'root_archives_before_retry' => 0))
      portal = File.join(workspace, 'retirement-portal.rb')
      File.write(portal, <<~RUBY)
        require 'json'
        control = #{control.inspect}
        File.open(#{log.inspect}, 'a') { |file| file.puts JSON.generate(ARGV) }
        state = JSON.parse(File.read(#{state_path.inspect}))
        failure = File.exist?(control) ? File.read(control) : nil
        case ARGV.first(2)
        when ['team', 'require-archive-ready']
          abort failure if %w[unknown-thread busy queued unresolved creating replacement removal].include?(failure)
          if failure == 'cleanup-preflight'
            proofs = File.readlines(#{log.inspect}).count { |line| JSON.parse(line).first(2) == ['team', 'require-archive-ready'] }
            abort 'cleanup preflight found an unknown conversation' if proofs >= 4
          end
        when ['team', 'archive']
          abort 'archive may not recycle members' unless ARGV.include?('--retained-only')
          state['members'] = %w[archived archived]
          File.write(#{state_path.inspect}, JSON.generate(state))
          abort 'lost team acknowledgement' if failure == 'team'
        when ['team', 'require-archived']
          abort 'members not archived' unless state['members'] == %w[archived archived]
          abort 'member final proof unavailable' if failure == 'team-proof'
        when ['thread', 'retire']
          abort 'root retired before members' unless state['members'] == %w[archived archived]
          abort 'ordinary archive was forced' if ARGV.include?('--force')
          if state['root'] != 'archived'
            state['root'] = 'archived'
            state['root_archives_before_retry'] += 1 if failure == 'root'
            File.write(#{state_path.inspect}, JSON.generate(state))
          end
          abort 'lost root acknowledgement' if failure == 'root'
        end
      RUBY
      runner = runner_for(workspace, authority_dir: File.join(workspace, '.authority'), env: {
        DevSession::ENV_PORTAL_COMMAND => [RbConfig.ruby, portal].shelljoin,
        DevSession::ENV_CODEX_SOCKET => '/run/test/codex.sock',
        DevSession::ENV_CODEX_VERSION => '0.160.0',
        'DEV_WORKSPACE_CODEX_HOME' => File.join(workspace, '.codex')
      })
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug)
      manifest['codex'] = {
        'thread_id' => 'root-thread', 'socket_path' => '/run/test/codex.sock',
        'client_version' => '0.160.0'
      }
      runner.send(:write_portal_manifest, slug, manifest)
      roster = runner.send(:direct_team_state_path, slug)
      FileUtils.mkdir_p(File.dirname(roster), mode: 0o700)
      File.write(roster, 'fake retained roster')
      commit_tracking(workspace, slug, lifecycle: 'complete')
      configure_workspace_origin(workspace)
      yield runner, workspace, slug, control, log
    end
  end
end
