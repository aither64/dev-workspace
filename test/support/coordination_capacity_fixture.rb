# frozen_string_literal: true

module CoordinationCapacityFixture
  def fill_coordination_admission_bytes(store)
    store.transaction do |data|
      remaining = WorkspaceCoordination::MAX_LEDGER - WorkspaceCoordination::RECOVERY_RESERVE -
        JSON.generate(data.merge('revision' => data['revision'] + 1)).bytesize - 1 - store.recovery_growth(data)
      loop do
        key = SecureRandom.uuid
        receipt = {'digest' => 'sha256:' + '0' * 64, 'result' => {'retained' => ''}}
        overhead = JSON.generate(key => receipt).bytesize - 2 + (data['receipts'].empty? ? 0 : 1)
        length = [WorkspaceCoordination::MAX_RESULT - 64, remaining - overhead - 128].min
        break unless length.positive?
        receipt['result']['retained'] = 'x' * length
        data['receipts'][key] = receipt
        remaining -= overhead + length
      end
    end
  end
end
