# frozen_string_literal: true

module WorkspaceCoordination
  class AdmissionBusy < Error
    def initialize(message)
      super(message, 75)
    end
  end

  # Retry acquisition only. Callers must never replay a mutation after its
  # callback has started. The ledger mutex owns logical write serialization.
  module Admission
    def self.acquire(file, timeout: 30, mode: File::LOCK_SH, diagnostics: nil)
      unless timeout.is_a?(Numeric) && timeout.finite? && timeout.between?(0, 3600)
        raise Error, 'admission timeout must be between 0 and 3600 seconds'
      end
      started = Process.clock_gettime(Process::CLOCK_MONOTONIC)
      delay = 0.05
      reported = false
      until file.flock(mode | File::LOCK_NB)
        elapsed = Process.clock_gettime(Process::CLOCK_MONOTONIC) - started
        if elapsed >= timeout
          raise AdmissionBusy, format('session lifecycle admission is busy after %.2fs; retry the same request', elapsed)
        end
        unless reported
          diagnostics&.puts 'waiting for session lifecycle admission; an exclusive lifecycle writer holds the guard'
          reported = true
        end
        sleep [delay, timeout - elapsed].min
        delay = [delay * 1.5, 0.5].min
      end
      Process.clock_gettime(Process::CLOCK_MONOTONIC) - started
    end
  end
end
