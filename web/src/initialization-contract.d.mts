export interface ElementContract {
  key: string;
  selector: string;
  all?: boolean;
  min?: number;
  max?: number;
  expected?: (
    values: Record<string, Element | Element[] | null>
  ) => { min: number; max: number };
}

export interface ElementContractCheck {
  key: string;
  selector: string;
  expected: string;
  actual: number;
  valid: boolean;
}

export function collectRequiredElements(
  root: ParentNode,
  contracts: ElementContract[]
): {
  values: Record<string, Element | Element[] | null>;
  checks: ElementContractCheck[];
  mismatches: ElementContractCheck[];
  valid: boolean;
};
