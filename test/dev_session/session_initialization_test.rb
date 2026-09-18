# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_start_seeds_the_goal_and_returns_json_with_a_shared_thread
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      out = StringIO.new
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Implement a useful feature.\n")
      tmux = ManagedTmux.new(slug, workspace:)
      portal_command = [
        RbConfig.ruby,
        '-e',
        "require 'json'; puts JSON.generate(threadId: 'thread-123')"
      ]
      session = DevSession::Tmux::Session.new(
        id: '$created',
        name: slug,
        mark: '1',
        slug:,
        workspace:
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) do |
          _slug, run_codex:, thread_id:, launch_codex:, identity_token:
        |
          raise 'missing shared thread' unless run_codex && thread_id == 'thread-123'
          raise 'Codex launched before the initial request persisted' if launch_codex
          raise 'missing journaled tmux identity' unless identity_token&.match?(/\A[0-9a-f]{64}\z/)

          session
        end

        define_method(:sync_slug) do |_slug, require_session:, session:|
          raise 'missing created session' unless require_session && session

          session
        end

        define_method(:revalidate_session!) do |expected|
          expected
        end

        define_method(:reconcile_native_client!) do |_slug, expected, **_keywords|
          expected
        end
      end
      runner = runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command:,
        portal_url: 'https://workspace.example.test'
      )

      runner.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        goal_file: goal,
        json: true
      )

      result = JSON.parse(out.string)
      assert_equal(slug, result['slug'])
      assert_equal('thread-123', result['threadId'])
      assert_equal("https://workspace.example.test/#{slug}/", result['url'])
      assert_includes(File.read(File.join(workspace, 'work', slug, 'plan.md')), 'Implement a useful feature.')
      assert_includes(File.read(File.join(workspace, 'work', slug, 'state.md')), 'initial request')
      assert_equal(
        {
          'state' => 'ready',
          'initial_goal_sent' => true,
          'goal_sha256' => Digest::SHA256.hexdigest('Implement a useful feature.')
        },
        YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
            .fetch('creation')
      )
    end
  end

  def test_portal_start_persists_receipt_bound_completion_for_the_exact_goal
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      out = StringIO.new
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Implement a useful feature.\n")
      tmux = ManagedTmux.new(slug, workspace:)
      portal_command = [
        RbConfig.ruby,
        '-e',
        "require 'json'; puts JSON.generate(threadId: 'thread-123')"
      ]
      session = DevSession::Tmux::Session.new(
        id: '$created',
        name: slug,
        mark: '1',
        slug:,
        workspace:, codex_thread_id: 'thread-123'
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:create_tmux_session) do |
          _slug, run_codex:, thread_id:, launch_codex:, identity_token:
        |
          raise 'missing shared thread' unless run_codex && thread_id == 'thread-123'
          raise 'Codex launched before the initial request persisted' if launch_codex
          raise 'missing journaled tmux identity' unless identity_token&.match?(/\A[0-9a-f]{64}\z/)

          session
        end

        define_method(:sync_slug) do |_slug, require_session:, session:|
          raise 'missing created session' unless require_session && session

          session
        end

        define_method(:revalidate_session!) do |expected|
          expected
        end

        define_method(:reconcile_native_client!) do |_slug, expected, **_keywords|
          expected
        end
      end
      runner = runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        out:,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command:,
        portal_url: 'https://workspace.example.test'
      )

      private_directory = File.join(workspace, '.portal-private')
      FileUtils.mkdir_p(private_directory, mode: 0o700)
      evidence_path = File.join(private_directory, 'complete.json')
      write_portal_creation_acceptance(workspace, slug, id: 'a' * 64, directory: private_directory)
      runner.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        goal_file: goal,
        json: true, creation_receipt_id: "a" * 64, creation_evidence: evidence_path
      )

      proof = JSON.parse(File.read(evidence_path))
      assert_equal('a' * 64, proof.fetch('receiptId'))
      assert_equal('thread-123', proof.fetch('threadId'))
      assert_equal(Digest::SHA256.hexdigest('Implement a useful feature.'), proof.fetch('goalSha256'))
      journal = JSON.parse(File.read(runner.send(:creation_journal_file, slug)))
      assert_equal(%w[goal_sha256 run_codex schema slug state], journal.keys.sort)
      result = JSON.parse(out.string)
      assert_equal(slug, result['slug'])
      assert_equal('thread-123', result['threadId'])
      assert_equal("https://workspace.example.test/#{slug}/", result['url'])
      assert_includes(File.read(File.join(workspace, 'work', slug, 'plan.md')), 'Implement a useful feature.')
      assert_includes(File.read(File.join(workspace, 'work', slug, 'state.md')), 'initial request')
      assert_equal(
        {
          'state' => 'ready',
          'initial_goal_sent' => true,
          'goal_sha256' => Digest::SHA256.hexdigest('Implement a useful feature.')
        },
        YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
            .fetch('creation')
      )
    end
  end

  def test_portal_plan_start_recovers_evidence_failure_after_source_archive_or_deletion
    %w[archive delete].each do |source_action|
      with_workspace do |workspace|
        source_slug = '2026-06-05-source'
        slug = '2026-06-06-plan-recovery'
        setup = runner_for(workspace)
        setup.ensure_tracking_files(source_slug)
        source = setup.send(:ensure_portal_manifest, source_slug)
        source['codex'] = { 'thread_id' => 'source-thread' }
        setup.send(:write_portal_manifest, source_slug, source)
        options = portal_creation_expectation(workspace, source_slug, destination: slug)
        goal = "Implement the following approved plan from session #{source_slug}.\n\nExact accepted plan."
        created = []
        delivered = []
        evidence_attempts = []
        session = DevSession::Tmux::Session.new(
          id: '$plan', name: slug, mark: '1', slug:, workspace:, codex_thread_id: 'plan-thread'
        )
        live = false
        runner_class = Class.new(DevSession::Runner) do
          define_method(:create_portal_thread) do |*_args, **_kwargs|
            created << 'plan-thread'
            'plan-thread'
          end
          define_method(:name_portal_thread) { |*_args| nil }
          define_method(:create_tmux_session) do |*_args, **kwargs|
            session.identity_token = kwargs.fetch(:identity_token)
            live = true
            session
          end
          define_method(:cleanup_session) { |_slug| live ? session : nil }
          define_method(:sync_slug) { |*_args, **_kwargs| session }
          define_method(:revalidate_session!) { |expected| expected }
          define_method(:reconcile_native_client!) { |_slug, expected, **_kwargs| expected }
          define_method(:send_portal_goal) do |_slug, _thread_id, goal_file, **_kwargs|
            delivered << read_goal(goal_file)
          end
          define_method(:write_portal_creation_evidence!) do |*args|
            evidence_attempts << true
            raise DevSession::Error, 'interrupted before completion evidence' if evidence_attempts.length == 1
            super(*args)
          end
        end
        runner = runner_class.new(workspace:, tmux: NullTmux.new, out: StringIO.new,
                                  err: StringIO.new, today: TODAY, env: {})
        arguments = { as_is: true, new: false, attach: false, run_codex: true, json: true,
                      exclusive: true, model: 'model-1', effort: 'high', goal_text: goal, **options }
        assert_raises(DevSession::Error) { runner.start(slug, **arguments) }
        journal_path = runner.send(:creation_journal_file, slug)
        original_journal = File.read(journal_path)
        assert_equal('ready', JSON.parse(original_journal).fetch('state'))
        refute(File.exist?(options.fetch(:creation_evidence)))
        source_path = File.join(workspace, 'work', source_slug)
        if source_action == 'archive'
          FileUtils.mkdir_p(File.join(workspace, 'archive'))
          File.rename(source_path, File.join(workspace, 'archive', source_slug))
        else
          FileUtils.rm_r(source_path)
        end
        assert_raises(DevSession::Error) { runner.start(slug, **arguments.merge(goal_text: 'A different plan')) }
        foreign_journal = JSON.parse(original_journal).merge('goal_sha256' => Digest::SHA256.hexdigest('A different plan'))
        File.write(journal_path, JSON.generate(foreign_journal))
        assert_raises(DevSession::Error) { runner.start(slug, **arguments) }
        File.write(journal_path, original_journal)
        runner.start(slug, **arguments)
        proof = JSON.parse(File.read(options.fetch(:creation_evidence)))
        assert_equal(options.fetch(:creation_receipt_id), proof.fetch('receiptId'))
        assert_equal('source-thread', proof.fetch('sourceThreadId'))
        assert_equal('plan-thread', proof.fetch('threadId'))
        assert_equal(Digest::SHA256.hexdigest(goal), proof.fetch('goalSha256'))
        assert_equal('model-1', proof.fetch('model'))
        assert_equal('high', proof.fetch('effort'))
        assert_equal(['plan-thread'], created)
        assert_equal([goal], delivered)
        assert_equal(original_journal, File.read(journal_path))
      end
    end
  end

  def test_portal_plan_start_binding_alone_still_requires_its_source
    %w[archive delete replace].each do |source_action|
      with_workspace do |workspace|
        source_slug = '2026-06-05-source'
        slug = '2026-06-06-plan-fresh'
        runner = runner_for(workspace)
        runner.ensure_tracking_files(source_slug)
        source = runner.send(:ensure_portal_manifest, source_slug)
        source['codex'] = { 'thread_id' => 'source-thread' }
        runner.send(:write_portal_manifest, source_slug, source)
        options = portal_creation_expectation(workspace, source_slug, destination: slug)
        receipt = runner.send(:creation_receipt_options, options.fetch(:creation_receipt_id), options.fetch(:creation_evidence),
                              expected_source: options.fetch(:expected_source), expected_source_thread: options.fetch(:expected_source_thread),
                              expected_source_identity: options.fetch(:expected_source_identity))
        runner.send(:prepare_portal_creation_receipt!, receipt, slug, 'start', goal: 'Exact plan', model: 'model-1', effort: 'high')
        binding = File.read(options.fetch(:creation_evidence) + '.request')
        source_path = File.join(workspace, 'work', source_slug)
        if source_action == 'archive'
          FileUtils.mkdir_p(File.join(workspace, 'archive'))
          File.rename(source_path, File.join(workspace, 'archive', source_slug))
        elsif source_action == 'delete'
          FileUtils.rm_r(source_path)
        else
          source['codex']['thread_id'] = 'replacement-thread'
          runner.send(:write_portal_manifest, source_slug, source)
        end
        assert_raises(DevSession::Error) do
          runner.start(slug, as_is: true, new: false, attach: false, run_codex: true, json: true,
                       exclusive: true, model: 'model-1', effort: 'high', goal_text: 'Exact plan', **options)
        end
        refute(File.exist?(runner.send(:creation_journal_file, slug)))
        refute(File.exist?(File.join(workspace, 'work', slug)))
        assert_equal(binding, File.read(options.fetch(:creation_evidence) + '.request'))
      end
    end
  end

  def test_portal_creation_retry_cannot_recreate_a_deleted_destination
    %w[new plan fork].each do |kind|
      with_workspace do |workspace|
        source_slug = '2026-06-05-source'
        slug = "2026-06-06-deleted-#{kind}"
        setup = runner_for(workspace)
        setup.ensure_tracking_files(source_slug)
        source = setup.send(:ensure_portal_manifest, source_slug)
        source['codex'] = { 'thread_id' => 'source-thread' }
        setup.send(:write_portal_manifest, source_slug, source)
        options = portal_creation_expectation(workspace, source_slug, destination: slug)
        options = options.reject { |key, _| key.to_s.start_with?('expected_source') } if kind == 'new'
        created = []
        delivered = []
        session = DevSession::Tmux::Session.new(
          id: '$deleted', name: slug, mark: '1', slug:, workspace:, codex_thread_id: 'deleted-thread'
        )
        runner_class = Class.new(DevSession::Runner) do
          define_method(:create_portal_thread) { |*_args, **_kwargs| created << kind; 'deleted-thread' }
          define_method(:create_portal_fork) { |*_args, **_kwargs| created << kind; 'deleted-thread' }
          define_method(:resolve_portal_fork_settings) { |*_args, **_kwargs| ['model-1', 'high'] }
          define_method(:name_portal_thread) { |*_args| nil }
          define_method(:create_tmux_session) do |*_args, **kwargs|
            session.identity_token = kwargs.fetch(:identity_token)
            session
          end
          define_method(:sync_slug) { |*_args, **_kwargs| session }
          define_method(:revalidate_session!) { |expected| expected }
          define_method(:reconcile_native_client!) { |_slug, expected, **_kwargs| expected }
          define_method(:send_portal_goal) { |_slug, _thread, path, **_kwargs| delivered << read_goal(path) }
          define_method(:write_portal_creation_evidence!) { |*_args| raise DevSession::Error, 'interrupted before receipt evidence' }
        end
        runner = runner_class.new(workspace:, tmux: NullTmux.new, out: StringIO.new,
                                  err: StringIO.new, today: TODAY,
                                  env: { 'XDG_STATE_HOME' => File.join(workspace, '.xdg-state'), 'PATH' => '' })
        arguments = { as_is: true, json: true, model: 'model-1', effort: 'high', **options }
        attempt = lambda do
          if kind == 'fork'
            runner.fork(source_slug, slug, **arguments)
          else
            runner.start(slug, new: false, attach: false, run_codex: true,
                         exclusive: true, goal_text: 'Exact accepted goal', **arguments)
          end
        end
        error = assert_raises(DevSession::Error, &attempt)
        assert_includes(error.message, 'interrupted before receipt evidence')
        assert_equal('ready', YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml'))).dig('creation', 'state'))
        refute(File.exist?(options.fetch(:creation_evidence)))
        binding_path = options.fetch(:creation_evidence) + '.request'
        original_binding = File.read(binding_path)
        original_time = File.mtime(binding_path)
        # The old generation can finish its fork journal without new receipt
        # evidence, then perform its normal deletion lifecycle.
        runner.send(:delete_fork_journal, slug) if kind == 'fork'
        removal = runner.send(:prepare_removal!, slug, force: false)
        %w[validated thread_retiring thread_retired clusters_released worktrees_removed runtime_retired].each do |phase|
          runner.send(:advance_removal!, slug, removal, phase)
        end
        worktrees = File.join(workspace, 'worktrees', slug)
        Dir.rmdir(worktrees) if File.directory?(worktrees)
        runner.send(:preserve_removed_state!, slug, removal, nil)
        runner.send(:advance_removal!, slug, removal, 'tracking_preserved')
        runner.send(:advance_removal!, slug, removal, 'tracking_committed')
        error = assert_raises(DevSession::Error, &attempt)
        assert_includes(error.message, 'unfinished')
        assert_equal(original_binding, File.read(binding_path))
        runner.send(:finalize_removal!, slug, removal)
        removal['removed_at'] = '2000-01-01T00:00:00Z'
        runner.send(:write_removal_metadata!, removal.fetch('recovery'), removal)
        error = assert_raises(DevSession::Error, &attempt)
        assert_includes(error.message, 'session was deleted')
        assert_equal(original_time, File.mtime(binding_path))
        assert_equal([kind], created)
        assert_equal(kind == 'fork' ? [] : ['Exact accepted goal'], delivered)
        refute(File.exist?(File.join(workspace, 'work', slug)))
        refute(File.exist?(runner.send(:creation_journal_file, slug)))
        # Retirement removes the cache. A queued worker cannot reconstruct
        # acceptance from its binding, the clock, or current deletion history.
        current_path = File.join(File.dirname(binding_path), "#{slug}.json")
        File.unlink(current_path)
        error = assert_raises(DevSession::Error, &attempt)
        assert_includes(error.message, 'no longer current')
        File.unlink(binding_path)
        error = assert_raises(DevSession::Error, &attempt)
        assert_includes(error.message, 'no longer current')
        refute(File.exist?(binding_path))
        assert_equal([kind], created)
        removal['removed_at'] = '2099-01-01T00:00:00Z'
        runner.send(:write_removal_metadata!, removal.fetch('recovery'), removal)
        fresh_options = portal_creation_expectation(workspace, source_slug, destination: slug, id: 'b' * 64)
        fresh_options = fresh_options.reject { |key, _| key.to_s.start_with?('expected_source') } if kind == 'new'
        fresh = runner.send(:creation_receipt_options, fresh_options.fetch(:creation_receipt_id), fresh_options.fetch(:creation_evidence),
                            expected_source: fresh_options[:expected_source], expected_source_thread: fresh_options[:expected_source_thread],
                            expected_source_identity: fresh_options[:expected_source_identity])
        runner.send(:prepare_portal_creation_receipt!, fresh, slug, kind == 'fork' ? 'fork' : 'start',
                    goal: kind == 'fork' ? nil : 'Fresh goal', model: 'model-1', effort: 'high')
        fresh_binding = fresh_options.fetch(:creation_evidence) + '.request'
        assert(File.file?(fresh_binding))
        original_history = fresh.fetch('binding').fetch('deletion_history_sha256')
        %w[2000-01-01T00:00:00Z 2099-01-01T00:00:00Z].each do |clock|
          removal['removed_at'] = clock
          runner.send(:write_removal_metadata!, removal.fetch('recovery'), removal)
          runner.send(:prepare_portal_creation_receipt!, fresh, slug, kind == 'fork' ? 'fork' : 'start',
                      goal: kind == 'fork' ? nil : 'Fresh goal', model: 'model-1', effort: 'high')
          assert_equal(original_history, fresh.fetch('binding').fetch('deletion_history_sha256'))
        end
        error = assert_raises(DevSession::Error, &attempt)
        assert_includes(error.message, 'no longer current')

      end
    end
  end

  def test_start_uses_one_private_snapshot_when_the_caller_goal_file_changes
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      original_request = 'Implement the original request.'
      goal = File.join(workspace, 'goal.txt')
      delivered = File.join(workspace, 'delivered.txt')
      snapshot_record = File.join(workspace, 'snapshot-record.txt')
      portal = File.join(workspace, 'fake-portal.rb')
      File.write(goal, "#{original_request}\n")
      File.write(portal, <<~RUBY)
        require 'json'
        case ARGV[1]
        when 'create'
          puts JSON.generate(threadId: 'thread-snapshot')
        when 'ensure-initial'
          input = ARGV.fetch(ARGV.index('--input-file') + 1)
          File.binwrite(#{delivered.dump}, File.binread(input))
          File.write(#{snapshot_record.dump}, "\#{input}\n\#{File.stat(input).mode & 0o777}\n")
        end
      RUBY
      session = DevSession::Tmux::Session.new(
        id: '$created', name: slug, mark: '1', slug:, workspace:
      )
      runner_class = Class.new(DevSession::Runner) do
        define_method(:prepare_creation_journal) do |*arguments, **keywords|
          journal = super(*arguments, **keywords)
          File.write(goal, "A replacement request that must be ignored.\n")
          journal
        end
        define_method(:create_tmux_session) { |*_arguments, **_keywords| session }
        define_method(:sync_slug) { |*_arguments, **_keywords| session }
        define_method(:revalidate_session!) { |_expected| session }
        define_method(:reconcile_native_client!) { |_slug, expected, **_keywords| expected }
      end
      runner = runner_class.new(
        workspace:,
        tmux: NullTmux.new,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [RbConfig.ruby, portal]
      )

      runner.start(
        slug,
        as_is: true,
        new: false,
        attach: false,
        run_codex: true,
        goal_file: goal,
        json: true,
        exclusive: true
      )

      assert_equal(original_request, File.binread(delivered))
      snapshot_path, snapshot_mode = File.readlines(snapshot_record, chomp: true)
      assert_equal(0o600, Integer(snapshot_mode))
      refute(File.exist?(snapshot_path))
      assert_includes(
        File.read(File.join(workspace, 'work', slug, 'plan.md')),
        original_request
      )
      refute_includes(
        File.read(File.join(workspace, 'work', slug, 'plan.md')),
        'replacement request'
      )
      journal = JSON.parse(File.read(runner.send(:creation_journal_file, slug)))
      assert_equal(Digest::SHA256.hexdigest(original_request), journal.fetch('goal_sha256'))
    end
  end

  def test_stopped_ready_session_rejects_a_recorded_thread_without_persisted_history
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'fake-portal.rb')
      File.write(portal, <<~RUBY)
        File.write(#{log.dump}, ARGV.join(' '))
        warn 'recorded thread is unavailable'
        exit 1
      RUBY
      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        codex_socket: '/run/test/codex.sock',
        codex_version: '0.152.1',
        codex_command: '/bin/true',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [RbConfig.ruby, portal]
      )
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:ensure_portal_manifest, slug, creation_journal: nil)
      manifest['codex'] = {
        'thread_id' => 'thread-ready',
        'socket_path' => '/run/test/codex.sock',
        'client_version' => '0.152.1'
      }
      runner.send(:write_portal_manifest, slug, manifest)

      error = assert_raises(DevSession::CommandError) do
        runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: true,
          json: true
        )
      end
      assert_includes(error.message, 'recorded thread is unavailable')
      command = File.read(log)
      assert_includes(command, 'thread require-materialized')
      assert_includes(command, '--thread-id thread-ready')
      assert_includes(command, "--cwd #{File.join(workspace, 'work', slug)}")
      refute_includes(command, 'thread create')
      unchanged = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('thread-ready', unchanged.dig('codex', 'thread_id'))
      assert_equal('ready', unchanged.dig('creation', 'state'))
    end
  end

  def test_ready_creation_replay_rejects_a_thread_without_persisted_history
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      log = File.join(workspace, 'portal.log')
      portal = File.join(workspace, 'fake-portal.rb')
      File.write(goal, "Implement a persistent conversation.\n")
      File.write(portal, <<~RUBY)
        File.write(#{log.dump}, ARGV.join(' '))
        warn 'recorded thread has no rollout'
        exit 1
      RUBY
      runner = DevSession::Runner.new(
        workspace:,
        tmux: NullTmux.new,
        codex_socket: '/run/test/codex.sock',
        codex_version: '0.152.1',
        codex_command: '/bin/true',
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [RbConfig.ruby, portal]
      )
      journal = runner.send(
        :prepare_creation_journal,
        slug,
        goal,
        exclusive: true,
        run_codex: true,
        model: nil,
        effort: nil
      )
      runner.ensure_tracking_files(slug)
      runner.send(:seed_goal, slug, goal)
      manifest = runner.send(:ensure_portal_manifest, slug, creation_journal: journal)
      manifest['codex'] = {
        'thread_id' => 'thread-ready',
        'socket_path' => '/run/test/codex.sock',
        'client_version' => '0.152.1'
      }
      manifest['creation']['state'] = 'ready'
      manifest['creation']['initial_goal_sent'] = true
      manifest['creation'].delete('initial_goal_attempted')
      manifest['schema'] = 1
      runner.send(:write_portal_manifest, slug, manifest)
      runner.send(:mark_creation_journal_ready, slug, journal)

      error = assert_raises(DevSession::CommandError) do
        runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: true,
          goal_file: goal,
          json: true,
          exclusive: true
        )
      end

      assert_includes(error.message, 'recorded thread has no rollout')
      assert_includes(File.read(log), 'thread require-materialized')
      unchanged = YAML.safe_load(File.read(File.join(workspace, 'work', slug, 'portal.yml')))
      assert_equal('ready', unchanged.dig('creation', 'state'))

      error = assert_raises(DevSession::CommandError) do
        runner.start(
          slug,
          as_is: true,
          new: false,
          attach: false,
          run_codex: true,
          goal_file: goal,
          json: true,
          exclusive: false
        )
      end

      assert_includes(error.message, 'recorded thread has no rollout')
      assert_includes(File.read(log), 'thread require-materialized')
    end
  end

  def test_start_refuses_to_retrofit_an_unshared_running_session
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      tmux = ManagedTmux.new(slug, workspace:)
      runner = DevSession::Runner.new(
        workspace:,
        tmux:,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: nil
      )
      runner.start(slug, as_is: true, new: false, attach: false, run_codex: false)

      shared_runner = DevSession::Runner.new(
        workspace:,
        tmux:,
        out: StringIO.new,
        err: StringIO.new,
        today: TODAY,
        env: {},
        portal_command: [
          RbConfig.ruby,
          '-e',
          "require 'json'; puts JSON.generate(threadId: 'unexpected')"
        ]
      )
      error = assert_raises(DevSession::Error) do
        shared_runner.start(slug, as_is: true, new: false, attach: false, run_codex: true)
      end
      assert_match(/no shared Codex thread/, error.message)
    end
  end

  def test_goal_seeding_is_stable_when_goal_contains_template_headings
    with_workspace do |workspace|
      slug = '2026-06-06-demo'
      goal = File.join(workspace, 'goal.txt')
      File.write(goal, "Keep this literal text:\n\n## Goal\n\n## Affected repositories\n")
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)

      runner.send(:seed_goal, slug, goal)
      first_plan = File.binread(File.join(workspace, 'work', slug, 'plan.md'))
      first_state = File.binread(File.join(workspace, 'work', slug, 'state.md'))
      runner.send(:seed_goal, slug, goal)

      assert_equal(first_plan, File.binread(File.join(workspace, 'work', slug, 'plan.md')))
      assert_equal(first_state, File.binread(File.join(workspace, 'work', slug, 'state.md')))
      assert_equal(1, first_plan.scan('Keep this literal text:').length)
      assert_includes(first_plan, "## Decisions\n")
      assert_includes(first_plan, "## Documentation\n")
      assert_operator(first_state.index('## Status'), :<, first_state.index('## Repositories'))
      assert_includes(first_state, "## Next actions\n")
      assert_includes(first_state, "## Documentation\n")
    end
  end

  def test_goal_seeding_resumes_previous_templates_without_rewriting_them
    [false, true].repeated_permutation(2) do |plan_seeded, state_seeded|
      with_workspace do |workspace|
        slug = '2026-06-06-demo'
        runner = runner_for(workspace)
        runner.ensure_tracking_files(slug)
        goal = File.join(workspace, 'goal.txt')
        File.write(goal, "Preserve this request.\n")
        plan = previous_tracking_template('plan', slug)
        state = previous_tracking_template('state', slug)
        seeded_plan = plan.sub("## Goal\n\n", "## Goal\n\nPreserve this request.\n\n")
        seeded_state = state.sub("## Status\n\n", "## Status\n\n- Session created with an initial request.\n\n")
        plan_path = File.join(workspace, 'work', slug, 'plan.md')
        state_path = File.join(workspace, 'work', slug, 'state.md')
        File.write(plan_path, plan_seeded ? seeded_plan : plan)
        File.write(state_path, state_seeded ? seeded_state : state)

        runner.send(:validate_creation_tracking_files!, slug)
        2.times { runner.send(:seed_goal, slug, goal) }

        assert_equal(seeded_plan, File.read(plan_path))
        assert_equal(seeded_state, File.read(state_path))
      end
    end
  end

  def test_goal_seeding_rejects_edited_current_and_previous_templates
    [false, true].product(%w[plan state]).each do |previous, kind|
      with_workspace do |workspace|
        slug = '2026-06-06-demo'
        runner = runner_for(workspace)
        runner.ensure_tracking_files(slug)
        goal = File.join(workspace, 'goal.txt')
        File.write(goal, "Keep operator edits.\n")
        path = File.join(workspace, 'work', slug, "#{kind}.md")
        content = previous ? previous_tracking_template(kind, slug) : File.read(path)
        edited = content + "\nOperator notes.\n"
        File.write(path, edited)

        assert_raises(DevSession::Error) { runner.send(:seed_goal, slug, goal) }
        assert_equal(edited, File.read(path))
      end
    end
  end

end
