# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_creation_capture_forwards_nested_stage_before_child_exit_and_drains_both_pipes
    Dir.mktmpdir('creation-progress-') do |directory|
      release = File.join(directory, 'release')
      forwarded = Queue.new
      errors = StringIO.new
      errors.define_singleton_method(:write) do |data|
        result = super(data)
        forwarded << true
        result
      end
      runner = DevSession::CommandRunner.new(out: StringIO.new, err: errors)
      input = 'input' * 30_000
      program = <<~'RUBY'
        require 'json'
        STDOUT.write('o' * 131_072)
        STDOUT.flush
        STDERR.write('diagnostic' * 16_384)
        STDERR.write("\x1eDEV_WORKSPACE_CREATION_PRO")
        STDERR.flush
        STDERR.write("GRESS/1 " + JSON.generate(stage: 'recovery_scan', event: 'begin', elapsedMs: 0) + "\n")
        STDERR.flush
        sleep 0.01 until File.exist?(ARGV.fetch(0))
        input = STDIN.read
        STDOUT.write(input)
      RUBY
      result = Thread.new do
        runner.with_timeout(5) do
          runner.capture([RbConfig.ruby, '-e', program, release], input:,
                         env: { DevSession::CREATION_PROGRESS_ENV => '1' })
        end
      end
      begin
        deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + 3
        sleep 0.005 while forwarded.empty? && Process.clock_gettime(Process::CLOCK_MONOTONIC) < deadline
        refute(forwarded.empty?, 'nested stage was buffered until child exit')
        assert(result.alive?, 'child exited before its release')
        File.write(release, '')
        stdout, stderr, status = result.value
        assert(status.success?)
        assert_equal('o' * 131_072 + input, stdout)
        assert_equal('diagnostic' * 16_384, stderr)
        assert_equal(1, errors.string.scan(DevSession::CREATION_PROGRESS_PREFIX).length)
        assert_includes(errors.string, 'recovery_scan')
      ensure
        File.write(release, '')
        result.join
      end
    end
  end

  def test_creation_capture_preserves_diagnostics_input_descriptors_and_failure_status
    Dir.mktmpdir('creation-progress-') do |directory|
      descriptor = File.open(File.join(directory, 'lock'), 'w+')
      descriptor.write('retained descriptor')
      descriptor.flush
      descriptor.rewind
      errors = StringIO.new
      runner = DevSession::CommandRunner.new(out: StringIO.new, err: errors)
      program = <<~'RUBY'
        require 'json'
        STDOUT.write(File.for_fd(ARGV.fetch(0).to_i).read)
        STDERR.write("\x1eDEV_WORKSPACE_CREATION_PROGRESS/1 " + JSON.generate(stage: 'conversation', event: 'begin', elapsedMs: 0) + "\n")
        STDERR.write("actual diagnostic\n")
        STDERR.write("\x1eDEV_WORKSPACE_CREATION_PROGRESS/1 bad\n")
        exit 7
      RUBY
      failure = assert_raises(DevSession::CommandError) do
        runner.capture([RbConfig.ruby, '-e', program, descriptor.fileno.to_s], pass_fds: [descriptor],
                       env: { DevSession::CREATION_PROGRESS_ENV => '1' })
      end
      assert_includes(failure.message, 'exit 7')
      assert_includes(failure.message, 'retained descriptor')
      assert_includes(failure.message, 'actual diagnostic')
      refute_includes(failure.message, '"stage":"conversation"')
      assert_includes(errors.string, '"stage":"conversation"')
    ensure
      descriptor&.close
    end
  end

  def test_creation_progress_decoder_bounds_rejected_frames_and_keeps_later_valid_frames
    errors = StringIO.new
    decoder = DevSession::CreationProgressDecoder.new(errors)
    prefix = DevSession::CREATION_PROGRESS_PREFIX
    decoder.feed(prefix + 'x' * 100_000 + "\n")
    decoder.feed(prefix + '{"stage":"prompt","stage":"terminal","event":"begin","elapsedMs":0}' + "\n")
    decoder.feed(prefix + '{"stage":"ready","event":"finish","elapsedMs":0}' + "\n")
    valid = prefix + '{"stage":"evidence","event":"finish","elapsedMs":12}' + "\n"
    valid.each_byte { |byte| decoder.feed(byte.chr) }
    decoder.feed(prefix + '{')
    diagnostics = decoder.finish
    assert_equal(valid, errors.string)
    assert_includes(diagnostics, 'omitted')
    assert(diagnostics.end_with?(prefix + '{'))
    assert_operator(diagnostics.bytesize, :<, 1000)
  end

  def test_creation_progress_opt_in_does_not_enter_runtime_environment
    with_workspace do |workspace|
      runner = runner_for(workspace, env: { DevSession::CREATION_PROGRESS_ENV => '1' })
      environment = runner.send(:session_environment, '2026-10-02-progress')
      refute(environment.key?(DevSession::CREATION_PROGRESS_ENV))
    end
  end
end
