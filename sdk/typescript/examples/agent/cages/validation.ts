// Validation cage placeholder.
//
// Production validation cages reproduce a parent finding to confirm
// it independently: replay the reproductionSteps, capture fresh
// evidence, file a child finding with validationProof.confirmed set
// accordingly. The starter agent does not implement this; the
// validation workflow path is exercised by purpose-built validation
// agents in production.

export async function runValidation(): Promise<void> {
  console.log('Validation cage is not implemented in the starter agent.');
  console.log('Production validation cages reproduce a finding to confirm it independently.');
}
