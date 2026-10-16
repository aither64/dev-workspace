# frozen_string_literal: true

require 'minitest/autorun'
require 'tmpdir'
require 'stringio'
require_relative '../libexec/workspace-coordination'
require_relative '../libexec/workspace-coordination-admission'

class CoordinationAdmissionTest < Minitest::Test
  def with_lock
    Dir.mktmpdir do |directory|
      path = File.join(directory, 'session.lock')
      File.open(path, 'w+', 0o600) { |file| yield path, file }
    end
  end

  def test_ordinary_admissions_share_the_guard
    with_lock do |path, first|
      WorkspaceCoordination::Admission.acquire(first, timeout: 0)
      File.open(path, 'r+') do |second|
        assert_operator WorkspaceCoordination::Admission.acquire(second, timeout: 0), :>=, 0
        File.open(path, 'r+') { |writer| refute writer.flock(File::LOCK_EX | File::LOCK_NB) }
      end
    end
  end

  def test_exclusive_lifecycle_admission_times_out_without_entering_callback
    with_lock do |path, writer|
      writer.flock(File::LOCK_EX)
      File.open(path, 'r+') do |reader|
        error = assert_raises(WorkspaceCoordination::AdmissionBusy) do
          WorkspaceCoordination::Admission.acquire(reader, timeout: 0)
          flunk 'admission callback must not start'
        end
        assert_equal 75, error.code
      end
    end
  end

  def test_acquisition_wakes_after_lifecycle_writer_releases
    with_lock do |path, writer|
      writer.flock(File::LOCK_EX)
      diagnostics = StringIO.new
      waiter = Thread.new do
        File.open(path, 'r+') { |reader| WorkspaceCoordination::Admission.acquire(reader, timeout: 1, diagnostics:) }
      end
      Thread.pass until diagnostics.string.include?('waiting')
      refute waiter.join(0.02)
      writer.flock(File::LOCK_UN)
      assert waiter.join(1)
      assert_equal 1, diagnostics.string.lines.length
    ensure
      waiter&.kill if waiter&.alive?
    end
  end

  def test_invalid_timeouts_refuse_before_acquisition
    with_lock do |_path, file|
      [-1, Float::NAN, Float::INFINITY, 3601, '30'].each do |timeout|
        assert_raises(WorkspaceCoordination::Error) { WorkspaceCoordination::Admission.acquire(file, timeout:) }
      end
    end
  end
end
