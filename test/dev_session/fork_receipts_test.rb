# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_portal_fork_persists_receipt_evidence_before_removing_its_journal
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination = '2026-06-06-proof'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      manifest = setup.send(:ensure_portal_manifest, source_slug)
      manifest['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, manifest)
      options = portal_creation_expectation(workspace, source_slug, destination: destination)
      session = DevSession::Tmux::Session.new(
        id: '$fork', name: destination, mark: '1', slug: destination,
        workspace:, socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-fork'
      )
      observed = []
      runner_class = Class.new(DevSession::Runner) do
        define_method(:resolve_portal_fork_settings) { |*_args, **_kwargs| ['model-1', 'high'] }
        define_method(:create_portal_fork) { |*_args, **_kwargs| 'thread-fork' }
        define_method(:name_portal_thread) { |*_args| nil }
        define_method(:create_tmux_session) do |*_args, **kwargs|
          session.identity_token = kwargs.fetch(:identity_token)
          session
        end
        define_method(:sync_slug) { |*_args, **_kwargs| session }
        define_method(:delete_fork_journal) do |slug|
          observed << JSON.parse(File.read(options.fetch(:creation_evidence)))
          raise 'journal removed before evidence' unless File.file?(fork_journal_file(slug))
          super(slug)
        end
      end
      runner = runner_class.new(workspace:, tmux: NullTmux.new, out: StringIO.new,
                                err: StringIO.new, today: TODAY, env: {})
      runner.fork(source_slug, destination, as_is: true, json: true,
                  model: 'model-1', effort: 'high', **options)
      assert_equal(1, observed.length)
      evidence = observed.fetch(0)
      assert_equal(options.fetch(:creation_receipt_id), evidence.fetch('receiptId'))
      assert_equal('thread-source', evidence.fetch('sourceThreadId'))
      assert_equal('thread-fork', evidence.fetch('threadId'))
      assert_equal('model-1', evidence.fetch('model'))
      assert_equal(File.stat(File.join(workspace, 'work', destination)).ino, evidence.fetch('trackingInode'))
      refute(File.exist?(setup.send(:fork_journal_file, destination)))
      assert_equal(0o600, File.stat(options.fetch(:creation_evidence)).mode & 0o777)
    end
  end

  def test_portal_fork_retries_an_evidence_failure_without_forking_twice
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      destination = '2026-06-06-proof'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      manifest = setup.send(:ensure_portal_manifest, source_slug)
      manifest['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, manifest)
      options = portal_creation_expectation(workspace, source_slug, destination: destination)
      session = DevSession::Tmux::Session.new(
        id: '$fork', name: destination, mark: '1', slug: destination,
        workspace:, socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-fork'
      )
      observed = []
      fork_calls = []
      evidence_attempts = []
      runner_class = Class.new(DevSession::Runner) do
        define_method(:resolve_portal_fork_settings) { |*_args, **_kwargs| ['model-1', 'high'] }
        define_method(:create_portal_fork) do |*_args, **_kwargs|
          fork_calls << true
          'thread-fork'
        end
        define_method(:write_portal_creation_evidence!) do |*args|
          evidence_attempts << true
          raise DevSession::Error, 'simulated evidence persistence failure' if evidence_attempts.length == 1
          super(*args)
        end
        define_method(:name_portal_thread) { |*_args| nil }
        define_method(:create_tmux_session) do |*_args, **kwargs|
          session.identity_token = kwargs.fetch(:identity_token)
          session
        end
        define_method(:sync_slug) { |*_args, **_kwargs| session }
        define_method(:delete_fork_journal) do |slug|
          observed << JSON.parse(File.read(options.fetch(:creation_evidence)))
          raise 'journal removed before evidence' unless File.file?(fork_journal_file(slug))
          super(slug)
        end
      end
      runner = runner_class.new(workspace:, tmux: NullTmux.new, out: StringIO.new,
                                err: StringIO.new, today: TODAY, env: {})
      error = assert_raises(DevSession::Error) do
        runner.fork(source_slug, destination, as_is: true, json: true,
                    model: 'model-1', effort: 'high', **options)
      end
      assert_includes(error.message, 'evidence persistence failure')
      assert(File.file?(setup.send(:fork_journal_file, destination)))
      refute(File.exist?(options.fetch(:creation_evidence)))
      wrong = options.merge(creation_receipt_id: 'b' * 64)
      assert_raises(DevSession::Error) do
        runner.fork(source_slug, destination, as_is: true, json: true,
                    model: 'model-1', effort: 'high', **wrong)
      end
      runner.fork(source_slug, destination, as_is: true, json: true,
                  model: 'model-1', effort: 'high', **options)
      assert_equal(1, fork_calls.length)
      assert_equal(1, observed.length)
      evidence = observed.fetch(0)
      assert_equal(options.fetch(:creation_receipt_id), evidence.fetch('receiptId'))
      assert_equal('thread-source', evidence.fetch('sourceThreadId'))
      assert_equal('thread-fork', evidence.fetch('threadId'))
      assert_equal('model-1', evidence.fetch('model'))
      assert_equal(File.stat(File.join(workspace, 'work', destination)).ino, evidence.fetch('trackingInode'))
      refute(File.exist?(setup.send(:fork_journal_file, destination)))
      assert_equal(0o600, File.stat(options.fetch(:creation_evidence)).mode & 0o777)
    end
  end

  def test_portal_fork_recovers_its_bound_journal_after_source_archive_or_deletion
    %w[archive delete].each do |source_action|
      with_workspace do |workspace|
        source_slug = '2026-06-05-source'
        destination = '2026-06-06-recovery'
        setup = runner_for(workspace)
        setup.ensure_tracking_files(source_slug)
        manifest = setup.send(:ensure_portal_manifest, source_slug)
        manifest['codex'] = { 'thread_id' => 'thread-source' }
        setup.send(:write_portal_manifest, source_slug, manifest)
        options = portal_creation_expectation(workspace, source_slug, destination: destination)
        session = DevSession::Tmux::Session.new(
          id: '$fork', name: destination, mark: '1', slug: destination,
          workspace:, socket_path: '/run/test/tmux.sock', codex_thread_id: 'thread-fork'
        )
        forks = []
        tmux_attempts = []
        runner_class = Class.new(DevSession::Runner) do
          define_method(:resolve_portal_fork_settings) { |*_args, **_kwargs| ['model-1', 'high'] }
          define_method(:create_portal_fork) do |*_args, **_kwargs|
            forks << true
            'thread-fork'
          end
          define_method(:name_portal_thread) { |*_args| nil }
          define_method(:create_tmux_session) do |*_args, **kwargs|
            tmux_attempts << true
            raise DevSession::Error, 'interrupted after journal and thread publication' if tmux_attempts.length == 1
            session.identity_token = kwargs.fetch(:identity_token)
            session
          end
          define_method(:sync_slug) { |*_args, **_kwargs| session }
        end
        runner = runner_class.new(workspace:, tmux: NullTmux.new, out: StringIO.new,
                                  err: StringIO.new, today: TODAY, env: {})
        assert_raises(DevSession::Error) do
          runner.fork(source_slug, destination, as_is: true, json: true,
                      model: 'model-1', effort: 'high', **options)
        end
        journal_path = setup.send(:fork_journal_file, destination)
        assert(File.file?(journal_path))
        assert(File.file?(options.fetch(:creation_evidence) + '.request'))
        source_path = File.join(workspace, 'work', source_slug)
        if source_action == 'archive'
          FileUtils.mkdir_p(File.join(workspace, 'archive'))
          File.rename(source_path, File.join(workspace, 'archive', source_slug))
        else
          FileUtils.rm_r(source_path)
        end
        assert_raises(DevSession::Error) do
          runner.fork(source_slug, destination, as_is: true, json: true,
                      model: 'model-1', effort: 'high', **options.merge(expected_source_thread: 'other-thread'))
        end
        assert_raises(DevSession::Error) do
          runner.fork(source_slug, destination, as_is: true, json: true,
                      model: 'model-1', effort: 'high', **options.merge(creation_receipt_id: 'b' * 64))
        end
        runner.fork(source_slug, destination, as_is: true, json: true,
                    model: 'model-1', effort: 'high', **options)
        proof = JSON.parse(File.read(options.fetch(:creation_evidence)))
        assert_equal(options.fetch(:creation_receipt_id), proof.fetch('receiptId'))
        assert_equal('thread-source', proof.fetch('sourceThreadId'))
        assert_equal('thread-fork', proof.fetch('threadId'))
        assert_equal(1, forks.length)
        refute(File.exist?(journal_path))
      end
    end
  end

  def test_portal_fork_refuses_a_replaced_source_before_creating_destination_state
    with_workspace do |workspace|
      source_slug = '2026-06-05-source'
      setup = runner_for(workspace)
      setup.ensure_tracking_files(source_slug)
      manifest = setup.send(:ensure_portal_manifest, source_slug)
      manifest['codex'] = { 'thread_id' => 'thread-source' }
      setup.send(:write_portal_manifest, source_slug, manifest)
      options = portal_creation_expectation(workspace, source_slug, destination: '2026-06-06-replaced')
      manifest['codex']['thread_id'] = 'replacement-thread'
      setup.send(:write_portal_manifest, source_slug, manifest)
      error = assert_raises(DevSession::Error) do
        setup.fork(source_slug, '2026-06-06-replaced', as_is: true, json: true, **options)
      end
      assert_includes(error.message, 'source session changed')
      refute(File.exist?(File.join(workspace, 'work', '2026-06-06-replaced')))
      refute(File.exist?(options.fetch(:creation_evidence) + '.request'))
    end
  end

  def test_portal_creation_binding_preserves_request_and_refuses_an_unrelated_journal
    with_workspace do |workspace|
      runner = runner_for(workspace)
      directory = File.join(workspace, '.portal-private')
      FileUtils.mkdir_p(directory, mode: 0o700)
      receipt = runner.send(:creation_receipt_options, 'a' * 64, File.join(directory, 'complete.json'),
                            expected_source: nil, expected_source_thread: nil, expected_source_identity: nil)
      write_portal_creation_acceptance(workspace, '2026-06-06-new', id: 'a' * 64, directory: directory)
      runner.send(:prepare_portal_creation_receipt!, receipt, '2026-06-06-new', 'start',
                  goal: 'Accepted request', model: 'model-1', effort: 'high')
      error = assert_raises(DevSession::Error) do
        runner.send(:prepare_portal_creation_receipt!, receipt, '2026-06-06-new', 'start',
                    goal: 'Changed request', model: 'model-1', effort: 'high')
      end
      assert_includes(error.message, 'does not match')
      second = runner.send(:creation_receipt_options, 'b' * 64, File.join(directory, 'other.json'),
                           expected_source: nil, expected_source_thread: nil, expected_source_identity: nil)
      write_portal_creation_acceptance(workspace, '2026-06-06-unrelated', id: 'b' * 64, directory: directory)
      runner.send(:prepare_start_journal!, '2026-06-06-unrelated')
      assert_raises(DevSession::Error) do
        runner.send(:prepare_portal_creation_receipt!, second, '2026-06-06-unrelated', 'start',
                    goal: 'Accepted request', model: 'model-1', effort: 'high')
      end
      refute(File.exist?(second.fetch('evidence') + '.request'))
    end
  end

  def test_portal_creation_requires_its_exact_current_receipt_even_with_a_binding
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-06-06-current-receipt'
      directory = File.join(workspace, '.portal-private')
      FileUtils.mkdir_p(directory, mode: 0o700)
      path = write_portal_creation_acceptance(workspace, slug, id: 'a' * 64, directory: directory)
      original = File.read(path)
      receipt = runner.send(:creation_receipt_options, 'a' * 64, File.join(directory, 'complete.json'),
                            expected_source: nil, expected_source_thread: nil, expected_source_identity: nil)
      prepare = lambda do
        runner.send(:prepare_portal_creation_receipt!, receipt, slug, 'start',
                    goal: 'Accepted request', model: 'model-1', effort: 'high')
      end
      prepare.call
      binding = File.read(receipt.fetch('evidence') + '.request')
      %w[missing replaced workspace slug schema missing-history invalid-history].each do |change|
        current = JSON.parse(original)
        case change
        when 'replaced' then current['receiptId'] = 'b' * 64
        when 'workspace' then current['workspace'] += '-different'
        when 'slug' then current['request']['slug'] = 'different-slug'
        when 'schema' then current['schema'] = 99
        when 'missing-history' then current.delete('deletionHistorySha256')
        when 'invalid-history' then current['deletionHistorySha256'] = 'not-a-digest'
        end
        File.write(path, JSON.generate(current))
        File.chmod(0o600, path)
        File.unlink(path) if change == 'missing'
        error = assert_raises(DevSession::Error, &prepare)
        assert_includes(error.message, 'no longer current')
        assert_equal(binding, File.read(receipt.fetch('evidence') + '.request'))
        refute(File.exist?(runner.send(:creation_journal_file, slug)))
      end
      File.write(path, original)
      File.chmod(0o600, path)
      prepare.call
      changed = JSON.parse(original).merge('deletionHistorySha256' => 'c' * 64)
      File.write(path, JSON.generate(changed))
      error = assert_raises(DevSession::Error, &prepare)
      assert_includes(error.message, 'does not match the recorded CLI request')
      assert_equal(binding, File.read(receipt.fetch('evidence') + '.request'))
    end
  end

end
